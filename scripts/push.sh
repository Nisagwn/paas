#!/usr/bin/env bash
# Simulates a signed GitHub push webhook against a local paas.
#
#   scripts/push.sh <owner/repo> <branch> [sha]
#
# Uses PAAS_GITHUB_WEBHOOK_SECRET and PAAS_URL (default http://localhost:8080).
set -euo pipefail

repo=${1:?usage: push.sh <owner/repo> <branch> [sha]}
branch=${2:?usage: push.sh <owner/repo> <branch> [sha]}
sha=${3:-$(head -c 20 /dev/urandom | od -An -tx1 | tr -d ' \n')}
url=${PAAS_URL:-http://localhost:8080}
secret=${PAAS_GITHUB_WEBHOOK_SECRET:?set PAAS_GITHUB_WEBHOOK_SECRET}

body=$(printf '{"ref":"refs/heads/%s","after":"%s","repository":{"full_name":"%s"},"head_commit":{"message":"test push %s"}}' \
  "$branch" "$sha" "$repo" "${sha:0:7}")
sig="sha256=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$secret" | awk '{print $NF}')"

curl -sS -X POST "$url/webhooks/github" \
  -H "Content-Type: application/json" \
  -H "X-GitHub-Event: push" \
  -H "X-Hub-Signature-256: $sig" \
  -d "$body"
echo
