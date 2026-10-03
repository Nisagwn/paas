#!/usr/bin/env bash
# Writes a docker config.json with a fresh ECR token (valid 12 h) into the
# Secret paas/ecr-docker-config. The control plane mounts it at
# /docker-config (DOCKER_CONFIG), where buildctl reads it and forwards the
# credentials to buildkitd for the push. Runs on the host (instance role via
# IMDS), triggered by paas-ecr-auth.timer every 6 h.
set -euo pipefail

# shellcheck disable=SC1091
. /etc/paas/bootstrap.env
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

token=$(aws ecr get-login-password --region "${AWS_REGION}")
auth=$(printf 'AWS:%s' "$token" | base64 -w0)

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
chmod 0600 "$tmp"
jq -n --arg reg "${ECR_REGISTRY}" --arg auth "$auth" \
  '{auths: {($reg): {auth: $auth}}}' >"$tmp"

/usr/local/bin/k3s kubectl -n paas create secret generic ecr-docker-config \
  --from-file=config.json="$tmp" --dry-run=client -o yaml |
  /usr/local/bin/k3s kubectl apply --server-side --force-conflicts \
    --field-manager=paas-ecr-auth -f -

echo "ecr-docker-config refreshed for ${ECR_REGISTRY}"
