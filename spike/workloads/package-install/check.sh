#!/usr/bin/env bash
# The install that was mid-download when saved must finish after the restore,
# in the same sandbox (same start time and installer process).
set -euo pipefail
b=$1 a=$2
jq -e -n --slurpfile b "$b" --slurpfile a "$a" '
  ($b[0]) as $b | ($a[0]) as $a |
  if $a.started != $b.started or $a.installer_pid != $b.installer_pid then error("sandbox restarted instead of resuming") else empty end
' >/dev/null
deadline=$(( $(date +%s) + 600 ))
while :; do
  state=$("$RUN" probe "$WORKLOAD")
  code=$(jq -r .exit_code <<<"$state")
  [[ $code != null ]] && break
  (( $(date +%s) < deadline )) || { echo "install still running 10 minutes after restore: $state" >&2; exit 1; }
  sleep 5
done
if [[ $code != 0 ]]; then
  echo "install failed after restore: $state" >&2
  "$RUN" exec "$WORKLOAD" tail -5 /spike/pip.log >&2 || true
  exit 1
fi
echo "ok: install finished after restore ($(jq .attempts <<<"$state") attempt(s), $(jq .bytes "$b") bytes in at checkpoint)"
