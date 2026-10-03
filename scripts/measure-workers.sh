#!/usr/bin/env bash
# Measures queue throughput against worker count with the dry-run pipeline
# (each deployment = 4 simulated stages of 500 ms). Needs Postgres (make db)
# and k6. Results: docs/measurements/workers-<n>.json (k6 summary export).
#
#   scripts/measure-workers.sh [pushes] [worker counts...]
set -euo pipefail
pushes=${1:-40}; shift || true
counts=${*:-1 2 4 8}
db=${PAAS_MEASURE_DATABASE_URL:-postgres://paas:paas@localhost:5432/paas_test?sslmode=disable}
out=docs/measurements
bin=$(mktemp -d)/paas$(go env GOEXE)
go build -o "$bin" ./cmd/paas
for w in $counts; do
  docker compose exec -T postgres psql -qU paas -d paas_test -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >/dev/null 2>&1
  PAAS_ADDR=:18383 PAAS_DATABASE_URL=$db PAAS_API_TOKEN=tok PAAS_GITHUB_WEBHOOK_SECRET=sec \
    PAAS_WORKERS=$w PAAS_POLL_INTERVAL=200ms "$bin" >/dev/null 2>&1 &
  pid=$!
  for _ in $(seq 1 50); do curl -sf localhost:18383/healthz >/dev/null && break; sleep 0.2; done
  start=$(date +%s%N)
  k6 run -q --summary-export "$out/workers-$w.json" -e PAAS_URL=http://localhost:18383 -e TOKEN=tok -e SECRET=sec \
    -e PUSHES="$pushes" -e VUS="$pushes" loadtest/webhook-burst.js >/dev/null 2>&1 || true
  end=$(date +%s%N)
  kill $pid; wait $pid 2>/dev/null || true
  echo "workers=$w pushes=$pushes wall_ms=$(( (end - start) / 1000000 ))"
done
