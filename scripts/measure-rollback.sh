#!/usr/bin/env bash
# Rollback API latency: alternates production between two ready deployments
# N times and prints p50/p95/max. Dry-run pipeline, so this is the control
# plane's share (database transaction + alias sync); on Kubernetes the alias
# Ingress update is added, but no pod restarts and no rebuild.
#
#   scripts/measure-rollback.sh [n]
set -euo pipefail
n=${1:-100}
db=${PAAS_MEASURE_DATABASE_URL:-postgres://paas:paas@localhost:5432/paas_test?sslmode=disable}
docker compose exec -T postgres psql -qU paas -d paas_test -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >/dev/null 2>&1
bin=$(mktemp -d)/paas$(go env GOEXE); go build -o "$bin" ./cmd/paas
PAAS_ADDR=:18484 PAAS_DATABASE_URL=$db PAAS_API_TOKEN=tok PAAS_GITHUB_WEBHOOK_SECRET=sec \
  PAAS_POLL_INTERVAL=200ms "$bin" >/dev/null 2>&1 &
pid=$!; trap 'kill $pid 2>/dev/null' EXIT
U=http://localhost:18484; H="Authorization: Bearer tok"
for _ in $(seq 1 50); do curl -sf $U/healthz >/dev/null && break; sleep 0.2; done
curl -sf -H "$H" -d '{"name":"blog","repo":"nisagwn/blog"}' $U/api/apps >/dev/null
for i in 1 2; do PAAS_URL=$U PAAS_GITHUB_WEBHOOK_SECRET=sec scripts/push.sh nisagwn/blog main >/dev/null; done
until [ "$(curl -s -H "$H" "$U/api/apps/blog/deployments" | grep -o '"status":"ready"' | wc -l)" -ge 2 ]; do sleep 0.3; done
ids=($(curl -s -H "$H" "$U/api/apps/blog/deployments" | grep -o '"id":[0-9]*' | cut -d: -f2))
for i in $(seq 1 "$n"); do
  curl -s -o /dev/null -w '%{time_total}\n' -H "$H" -d "{\"deployment_id\":${ids[$((i % 2))]}}" $U/api/apps/blog/rollback
done | sort -n | awk '{a[NR]=$1*1000} END {printf "n=%d p50=%.1fms p95=%.1fms max=%.1fms\n", NR, a[int(NR*0.5)], a[int(NR*0.95)], a[NR]}'
