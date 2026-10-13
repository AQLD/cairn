#!/usr/bin/env bash
# E0 gate: save a workload on host A, move the save to host B, resume it there.
# Runs on your machine; drives both hosts over SSH as root.
#
#   cross.sh sync A B                  copy spike/ to both hosts and run setup
#   cross.sh run A B WORKLOAD          one workload, A -> B
#   cross.sh all A B                   every workload, A -> B and B -> A
#
# A and B are SSH targets (IPs or hostnames). Results append to spike/results/runs.jsonl.
# The save travels directly from A to B (SSH agent forwarding), as it would between servers.
set -euo pipefail

SPIKE_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REMOTE=/root/spike
WORKDIR=/var/lib/cairn-spike
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ServerAliveInterval=15)

log() { echo "[$(date +%H:%M:%S)] $*" >&2; }
on() { local h=$1; shift; ssh "${SSH_OPTS[@]}" "root@$h" "$@"; }
now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

cmd_sync() {
  for h in "$@"; do
    log "$h: syncing spike/ and running setup"
    rsync -az --delete --exclude results -e "ssh ${SSH_OPTS[*]}" "$SPIKE_DIR/" "root@$h:$REMOTE/"
    on "$h" "$REMOTE/run.sh setup"
  done
}

cmd_run() {
  local a=$1 b=$2 w=$3 tmp ck rs_ verdict detail t0 t1
  tmp=$(mktemp -d)
  log "$w: $a -> $b"

  on "$a" "$REMOTE/run.sh prepare $w && $REMOTE/run.sh start $w && $REMOTE/run.sh warm $w"
  on "$a" "$REMOTE/run.sh probe $w" > "$tmp/before.json"
  ck=$(on "$a" "$REMOTE/run.sh checkpoint $w")
  log "$w: checkpoint on $a: $(jq -c . <<<"$ck")"

  # Move the bundle (unchanged root filesystem) and the checkpoint image A -> B.
  local b_addr; b_addr=$(on "$b" "hostname -I | awk '{print \$1}'")
  t0=$(now_ms)
  ssh -A "${SSH_OPTS[@]}" "root@$a" \
    "tar -C $WORKDIR -cf - bundles/$w images/$w | zstd -q -T0 | ssh -o StrictHostKeyChecking=accept-new root@$b_addr 'mkdir -p $WORKDIR && rm -rf $WORKDIR/bundles/$w $WORKDIR/images/$w && zstd -q -d | tar -x -C $WORKDIR'"
  t1=$(now_ms)
  on "$a" "$REMOTE/run.sh stop $w" || true

  if rs_=$(on "$b" "$REMOTE/run.sh restore $w" 2>"$tmp/restore.err"); then
    sleep 3
    on "$b" "$REMOTE/run.sh probe $w" > "$tmp/after.json" || echo '{}' > "$tmp/after.json"
    on "$b" "cat > /tmp/before-$w.json" < "$tmp/before.json"
    on "$b" "cat > /tmp/after-$w.json" < "$tmp/after.json"
    if detail=$(on "$b" "WORKLOAD=$w RUN=$REMOTE/run.sh $REMOTE/run.sh check $w /tmp/before-$w.json /tmp/after-$w.json" 2>&1); then
      verdict=pass
    else
      verdict=fail
    fi
  else
    rs_='{"restore_ms":null}'
    verdict=fail
    detail="restore failed: $(tail -5 "$tmp/restore.err")"
  fi
  on "$b" "$REMOTE/run.sh stop $w" || true

  jq -n -c \
    --arg w "$w" --arg verdict "$verdict" --arg detail "$detail" --arg mode cross \
    --argjson from "$(on "$a" "$REMOTE/run.sh host-info" | jq -c 'del(.cpu_flags)')" \
    --argjson to "$(on "$b" "$REMOTE/run.sh host-info" | jq -c 'del(.cpu_flags)')" \
    --argjson ck "$ck" --argjson rs "$rs_" --argjson transfer_ms $((t1 - t0)) \
    '{workload:$w, mode:$mode, verdict:$verdict, detail:$detail, from:$from, to:$to, transfer_ms:$transfer_ms} + $ck + $rs' \
    | tee -a "$SPIKE_DIR/results/runs.jsonl"
  rm -rf "$tmp"
  [[ $verdict == pass ]]
}

cmd_all() {
  local a=$1 b=$2 failed=0
  for w in $(ls "$SPIKE_DIR/workloads"); do
    cmd_run "$a" "$b" "$w" || failed=1
    cmd_run "$b" "$a" "$w" || failed=1
  done
  return $failed
}

case ${1:-} in
  sync) shift; [[ $# -ge 1 ]] || { echo "usage: $0 sync HOST..." >&2; exit 2; }; cmd_sync "$@" ;;
  run) shift; [[ $# -eq 3 ]] || { echo "usage: $0 run A B WORKLOAD" >&2; exit 2; }; cmd_run "$@" ;;
  all) shift; [[ $# -eq 2 ]] || { echo "usage: $0 all A B" >&2; exit 2; }; cmd_all "$@" ;;
  *) sed -n '2,11p' "$0"; exit 2 ;;
esac
