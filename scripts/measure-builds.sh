#!/usr/bin/env bash
# Build time and image size for each example, with the Dockerfile the
# platform generates: cold (no layer cache), warm (nothing changed) and
# after a source change (dependency layers cached). Needs Docker.
#
#   scripts/measure-builds.sh > docs/measurements/builds.tsv
set -euo pipefail
ms() { echo $(( $(date +%s%N) / 1000000 )); }
work=$(mktemp -d)
printf 'example\tcold_ms\twarm_ms\tchange_ms\timage_mb\n'
for ex in node-hello go-hello static-site; do
  cp -r "examples/$ex" "$work/$ex"
  go run ./cmd/paas-dockerfile "$work/$ex" > "$work/$ex.Dockerfile" 2>/dev/null
  b() { docker buildx build -q --load -f "$work/$ex.Dockerfile" -t "measure/$ex" "$@" "$work/$ex" >/dev/null; }
  t=$(ms); b --no-cache; cold=$(( $(ms) - t ))
  t=$(ms); b;            warm=$(( $(ms) - t ))
  # A code change: the last file in the context is touched.
  f=$(ls "$work/$ex" | grep -v -e package -e go.mod | head -1); echo "// change $RANDOM" >> "$work/$ex/$f"
  t=$(ms); b;            change=$(( $(ms) - t ))
  size=$(docker image inspect "measure/$ex" --format '{{.Size}}')
  printf '%s\t%s\t%s\t%s\t%s\n' "$ex" "$cold" "$warm" "$change" "$(( size / 1000000 ))"
done
rm -rf "$work"
