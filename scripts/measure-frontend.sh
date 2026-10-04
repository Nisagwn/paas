#!/usr/bin/env bash
# Faz 14: warm build time with and without the "# syntax=docker/dockerfile:1"
# directive, alternating runs so both see the same machine load. Needs Docker.
#
#   scripts/measure-frontend.sh [runs] > docs/measurements/frontend.tsv
set -euo pipefail
runs=${1:-7}
ms() { echo $(( $(date +%s%N) / 1000000 )); }
median() { sort -n | awk '{a[NR]=$1} END {print (NR%2 ? a[(NR+1)/2] : int((a[NR/2]+a[NR/2+1])/2))}'; }
work=$(mktemp -d)
printf 'example\twith_syntax_ms\twithout_syntax_ms\n'
for ex in node-hello go-hello static-site python-hello ruby-hello java-hello; do
  cp -r "examples/$ex" "$work/$ex"
  go run ./cmd/paas-dockerfile "$work/$ex" > "$work/$ex.plain" 2>/dev/null
  { echo '# syntax=docker/dockerfile:1'; cat "$work/$ex.plain"; } > "$work/$ex.syntax"
  b() { docker buildx build -q --load -f "$work/$ex.$1" -t "measure/$ex-$1" "$work/$ex" >/dev/null; }
  b plain; b syntax # fill the cache
  : > "$work/s"; : > "$work/p"
  for _ in $(seq "$runs"); do
    t=$(ms); b syntax; echo $(( $(ms) - t )) >> "$work/s"
    t=$(ms); b plain;  echo $(( $(ms) - t )) >> "$work/p"
  done
  printf '%s\t%s\t%s\n' "$ex" "$(median < "$work/s")" "$(median < "$work/p")"
  docker image rm -f "measure/$ex-plain" "measure/$ex-syntax" >/dev/null
done
rm -rf "$work"
