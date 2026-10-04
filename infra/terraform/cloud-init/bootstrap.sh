#!/usr/bin/env bash
# Node bootstrap, run once by cloud-init (and safe to re-run by hand):
#   k3s (+ bundled Traefik) → Helm → cert-manager → secrets → ECR auth →
#   manifests in /opt/paas/manifests.
# Settings come from /etc/paas/bootstrap.env (written by Terraform).
set -euo pipefail

# shellcheck disable=SC1091
. /etc/paas/bootstrap.env

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
MANIFESTS=/opt/paas/manifests
ARCH=arm64

log() { echo "[paas-bootstrap $(date -u +%H:%M:%S)] $*"; }

# retry N cmd...: retries a command with a 5 s pause.
retry() {
  local n=$1; shift
  for ((i = 1; i <= n; i++)); do
    "$@" && return 0
    sleep 5
  done
  log "giving up: $*"
  return 1
}

# --- 1. Kernel: allow rootless BuildKit ------------------------------------
# Ubuntu 24.04 blocks unprivileged user namespaces through AppArmor, which
# rootlesskit (moby/buildkit:*-rootless) needs.
log "sysctl: unprivileged user namespaces"
cat >/etc/sysctl.d/90-paas.conf <<'EOF'
kernel.apparmor_restrict_unprivileged_userns = 0
# Traefik, buildkitd and many pods watch files.
fs.inotify.max_user_instances = 1024
fs.inotify.max_user_watches = 524288
EOF
sysctl --system >/dev/null

# --- 2. AWS CLI v2 (used by paas-ecr-auth) ------------------------------
if ! command -v aws >/dev/null; then
  log "installing AWS CLI v2"
  tmp=$(mktemp -d)
  retry 5 curl -fsSL -o "$tmp/awscli.zip" "https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip"
  unzip -q "$tmp/awscli.zip" -d "$tmp"
  "$tmp/aws/install" --update
  rm -rf "$tmp"
fi

# --- 3. kubelet image credential provider for ECR ---------------------------
# k3s enables the kubelet credential provider when both
# /var/lib/rancher/credentialprovider/bin and .../config.yaml exist at start.
# The binary uses the instance role (IMDS from the host) to get ECR tokens,
# so pods need no imagePullSecrets for *.dkr.ecr.*.amazonaws.com images.
CP_BIN=/var/lib/rancher/credentialprovider/bin/ecr-credential-provider
if [ ! -x "$CP_BIN" ]; then
  log "installing ecr-credential-provider ${ECR_CREDENTIAL_PROVIDER_VERSION}"
  mkdir -p "$(dirname "$CP_BIN")"
  retry 5 curl -fsSL -o "$CP_BIN" \
    "https://artifacts.k8s.io/binaries/cloud-provider-aws/${ECR_CREDENTIAL_PROVIDER_VERSION}/linux/${ARCH}/ecr-credential-provider-linux-${ARCH}"
  chmod 0755 "$CP_BIN"
fi

# --- 4. k3s -----------------------------------------------------------------
if ! systemctl is-active --quiet k3s; then
  log "installing k3s ${K3S_VERSION}"
  retry 5 curl -fsSL -o /tmp/k3s-install.sh https://get.k3s.io
  INSTALL_K3S_VERSION="${K3S_VERSION}" sh /tmp/k3s-install.sh server \
    --tls-san "${PUBLIC_IP}" \
    --tls-san "${DOMAIN}" \
    --write-kubeconfig-mode 0600
fi
ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl 2>/dev/null || true

log "waiting for the node to become Ready"
retry 60 kubectl get nodes >/dev/null
kubectl wait --for=condition=Ready node --all --timeout=300s

# --- 5. Helm + cert-manager -------------------------------------------------
if ! command -v helm >/dev/null; then
  log "installing Helm"
  retry 5 curl -fsSL -o /tmp/get-helm-3 https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3
  HELM_INSTALL_DIR=/usr/local/bin bash /tmp/get-helm-3
fi

log "installing cert-manager ${CERT_MANAGER_VERSION}"
retry 5 helm upgrade --install cert-manager cert-manager \
  --repo https://charts.jetstack.io \
  --version "${CERT_MANAGER_VERSION}" \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true \
  --set 'extraArgs={--dns01-recursive-nameservers-only,--dns01-recursive-nameservers=1.1.1.1:53\,8.8.8.8:53}' \
  --wait --timeout 10m

# The controller needs the instance role for Route 53 (ambient credentials).
# IMDS has a hop limit of 1, so only host-network processes reach it: run the
# controller in the host network namespace. The chart has no value for this,
# so it is patched (re-run this script after a `helm upgrade`).
log "cert-manager controller → hostNetwork"
kubectl -n cert-manager patch deployment cert-manager --type merge -p \
  '{"spec":{"template":{"spec":{"hostNetwork":true,"dnsPolicy":"ClusterFirstWithHostNet"}}}}'
kubectl -n cert-manager rollout status deployment/cert-manager --timeout=300s

# --- 6. Wait for Traefik's CRDs (k3s installs Traefik asynchronously) ---------
log "waiting for Traefik CRDs"
retry 120 kubectl get crd ingressroutes.traefik.io tlsstores.traefik.io middlewares.traefik.io >/dev/null
kubectl wait --for=condition=Established --timeout=120s \
  crd/ingressroutes.traefik.io crd/tlsstores.traefik.io crd/middlewares.traefik.io

# --- 7. Namespace + secrets (generated here, never in Terraform state) -------
kubectl apply -f "${MANIFESTS}/00-namespace.yaml"

if ! kubectl -n paas get secret postgres >/dev/null 2>&1; then
  log "generating Postgres credentials"
  kubectl -n paas create secret generic postgres \
    --from-literal=username=paas \
    --from-literal=password="$(openssl rand -hex 24)" \
    --from-literal=database=paas
fi

if ! kubectl -n paas get secret paas >/dev/null 2>&1; then
  log "generating control plane secrets"
  pg_pass=$(kubectl -n paas get secret postgres -o jsonpath='{.data.password}' | base64 -d)
  kubectl -n paas create secret generic paas \
    --from-literal=database-url="postgres://paas:${pg_pass}@postgres.paas.svc.cluster.local:5432/paas?sslmode=disable" \
    --from-literal=api-token="$(openssl rand -hex 32)" \
    --from-literal=github-webhook-secret="$(openssl rand -hex 32)" \
    --from-literal=env-key="$(openssl rand -base64 32)"
fi

# Faz 10: AES-256 key for app env values. Added to an existing Secret only
# when missing; never regenerated (losing it loses every encrypted value).
if [ -z "$(kubectl -n paas get secret paas -o jsonpath='{.data.env-key}')" ]; then
  log "generating env encryption key"
  kubectl -n paas patch secret paas --type merge \
    -p "{\"data\":{\"env-key\":\"$(openssl rand -base64 32 | tr -d '\n' | base64 -w0)\"}}"
fi

# --- 8. ECR credentials for buildctl (refreshed every 6 h) -------------------
log "ECR auth for buildctl"
systemctl daemon-reload
systemctl enable --now paas-ecr-auth.timer
retry 12 /usr/local/sbin/paas-ecr-auth

# --- 9. Platform manifests ----------------------------------------------------
log "applying manifests"
# cert-manager's webhook may need a moment before it admits Issuers.
retry 24 kubectl apply -f "${MANIFESTS}"

log "done. Certificate status: kubectl -n kube-system get certificate wildcard"
