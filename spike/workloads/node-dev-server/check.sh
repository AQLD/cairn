#!/usr/bin/env bash
# Same server process still serving, and its file watcher survived the restore.
set -euo pipefail
b=$1 a=$2
jq -e -n --slurpfile b "$b" --slurpfile a "$a" '
  ($b[0]) as $b | ($a[0]) as $a |
  if   $a.pid != $b.pid         then error("pid changed: server restarted")
  elif $a.started != $b.started then error("start time changed: server restarted")
  elif $a.hits <= $b.hits       then error("hit counter did not advance")
  else true end
' >/dev/null

# Edit a watched file inside the restored sandbox; the watcher must notice.
before_reloads=$(jq .reloads "$a")
"$RUN" exec "$WORKLOAD" sh -c 'echo "export const v = 2" > /app/src/index.js'
sleep 1
after_reloads=$("$RUN" probe "$WORKLOAD" | jq .reloads)
if (( after_reloads <= before_reloads )); then
  echo "file watcher did not fire after restore (reloads $before_reloads -> $after_reloads)" >&2
  exit 1
fi
echo "ok: same server pid, hits $(jq .hits "$b") -> $(jq .hits "$a"), watcher fired after restore"
