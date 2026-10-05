#!/usr/bin/env bash
# Local Kubernetes for end-to-end testing: a k3d cluster (k3s + Traefik) with
# its own registry, ports 80 → Traefik. Apps are reachable at
# http://<host>.localtest.me (*.localtest.me resolves to 127.0.0.1).
#
#   scripts/k3d-up.sh          # create
#   scripts/k3d-up.sh down     # delete cluster and registry
#
# Then run the control plane against it (see README, "Yerel Kubernetes"):
#   PAAS_BUILDER=docker PAAS_REGISTRY=localhost:5111
#   PAAS_DEPLOYER=kubernetes PAAS_INGRESS_TLS=false PAAS_DOMAIN=localtest.me
set -euo pipefail
cluster=paas
registry=paas-registry
port=5111

if [ "${1:-}" = down ]; then
  k3d cluster delete "$cluster" || true
  k3d registry delete "k3d-$registry" || true
  exit 0
fi

# The builder pushes to localhost:5111 from the host; inside the cluster
# "localhost" is the node itself, so containerd mirrors that name to the
# registry container on the k3d network.
cfg=$(mktemp)
cat > "$cfg" <<EOF
mirrors:
  "localhost:$port":
    endpoint:
      - http://k3d-$registry:5000
EOF
[ "$(uname -o 2>/dev/null)" = Msys ] && cfg=$(cygpath -w "$cfg")

k3d registry list | grep -q "k3d-$registry" || k3d registry create "$registry" --port "0.0.0.0:$port"
k3d cluster create "$cluster" --agents 0 \
  --registry-use "k3d-$registry:$port" --registry-config "$cfg" \
  -p "80:80@loadbalancer" --wait

# k3d writes https://host.docker.internal:<port> into kubeconfig; with some
# VPN clients (e.g. Cloudflare WARP) that name resolves to an unreachable
# address, so point kubectl at the loopback port instead.
api_port=$(docker port "k3d-$cluster-serverlb" 6443/tcp | head -1 | sed 's/.*://')
kubectl config set-cluster "k3d-$cluster" --server "https://127.0.0.1:$api_port" >/dev/null
kubectl get nodes
echo "ready: registry localhost:$port, apps at http://<host>.localtest.me"
# Scale to zero with the control plane on the host: Traefik reaches the
# activator at the host's address inside the cluster (host.k3d.internal).
# Per-service Traefik metrics for idle detection and analytics (Faz 19).
kubectl apply -f "$(dirname "$0")/../infra/k8s/05-traefik-config.yaml"
host_ip=$(kubectl -n kube-system get configmap coredns -o jsonpath='{.data.NodeHosts}' | awk '/host.k3d.internal/ {print $1}')
echo "scale to zero: PAAS_ACTIVATOR_IP=$host_ip PAAS_ACTIVATOR_UPSTREAM=http://127.0.0.1:80"
