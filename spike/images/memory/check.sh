#!/usr/bin/env bash
# Same process, still counting, and every byte of its memory came back.
set -euo pipefail
b=$1 a=$2
jq -e -n --slurpfile b "$b" --slurpfile a "$a" '
  ($b[0]) as $b | ($a[0]) as $a |
  if   $a.pid != $b.pid         then error("pid changed: process restarted")
  elif $a.started != $b.started then error("start time changed: process restarted")
  elif $a.counter <= $b.counter then error("counter did not advance")
  else true end
' >/dev/null
state=$("$RUN" exec "$WORKLOAD" /spike/probe verify)
if [[ $(jq -r .verified_sha <<<"$state") != $(jq -r .baseline_sha <<<"$state") ]]; then
  echo "memory differs after restore: $(jq -c '{baseline_sha, verified_sha}' <<<"$state")" >&2
  exit 1
fi
echo "ok: $(jq .mem_mb <<<"$state") MB of memory identical after restore (sha256 $(jq -r .baseline_sha <<<"$state" | cut -c1-12)), same pid"
