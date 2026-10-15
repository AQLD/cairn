#!/usr/bin/env bash
# E0 gate: save a workload on host A, move the save to host B, resume it there.
# Runs on your machine and drives both hosts over SSH. The same script works
# for the local dev VMs and the DigitalOcean droplets; only the settings differ.
#
#   cross.sh sync A B                  put the repo on both hosts and run setup
#   cross.sh run A B WORKLOAD          one workload, A -> B
#   cross.sh all A B                   every workload, A -> B and B -> A
#
# A and B are SSH destinations (root@203.0.113.5, or an alias from an ssh config).
# Settings (environment):
#   CAIRN_SSH_CONFIG    ssh config file to use (the dev VMs provide one)
#   CAIRN_REMOTE_REPO   repo path on the hosts (default /root/cairn)
#   CAIRN_SYNC          rsync | none   (none when the repo is already mounted; default rsync)
#   CAIRN_TRANSFER      direct | relay (direct: A streams to B over the private network,
#                       needs ssh-agent forwarding; relay: through this machine; default direct)
# Results append to spike/results/runs.jsonl.
set -euo pipefail

SPIKE_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_DIR=$(cd "$SPIKE_DIR/.." && pwd)
REMOTE_REPO=${CAIRN_REMOTE_REPO:-/root/cairn}
REMOTE=$REMOTE_REPO/spike
WORKDIR=/var/lib/cairn-spike
SYNC=${CAIRN_SYNC:-rsync}
TRANSFER=${CAIRN_TRANSFER:-direct}

SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ServerAliveInterval=15 -o BatchMode=yes)
[[ -n ${CAIRN_SSH_CONFIG:-} ]] && SSH_OPTS=(-F "$CAIRN_SSH_CONFIG" "${SSH_OPTS[@]}")

log() { echo "[$(date +%H:%M:%S)] $*" >&2; }
now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

# on HOST CMD: run CMD as root on HOST (sudo is a no-op for root).
on() {
  local h=$1; shift
  ssh "${SSH_OPTS[@]}" "$h" "sudo -n bash -c $(printf %q "$*")"
}

cmd_sync() {
  for h in "$@"; do
    if [[ $SYNC == rsync ]]; then
      log "$h: copying repo to $REMOTE_REPO"
      on "$h" "mkdir -p $REMOTE_REPO"
      rsync -az --delete --rsync-path="sudo -n rsync" \
        --exclude .git --exclude bin --exclude data --exclude 'spike/results/*.jsonl' \
        -e "ssh ${SSH_OPTS[*]}" "$REPO_DIR/" "$h:$REMOTE_REPO/"
    fi
    log "$h: setup"
    on "$h" "$REMOTE/run.sh setup"
  done
}

# move WORKLOAD's bundle and checkpoint image from A to B
move() {
  local a=$1 b=$2 w=$3 pack unpack
  pack="tar -C $WORKDIR -cf - bundles/$w images/$w | zstd -q -T0"
  unpack="mkdir -p $WORKDIR && rm -rf $WORKDIR/bundles/$w $WORKDIR/images/$w && zstd -q -d | tar -x -C $WORKDIR"
  if [[ $TRANSFER == relay ]]; then
    on "$a" "$pack" | on "$b" "$unpack"
  else
    # A streams straight to B's private address; the inner ssh runs as A's login
    # user so the forwarded agent is available, then becomes root on B.
    local b_login=${b%@*} b_addr
    [[ $b_login == "$b" ]] && b_login=root
    b_addr=$(on "$b" "hostname -I | awk '{print \$1}'")
    ssh -A "${SSH_OPTS[@]}" "$a" \
      "sudo -n bash -c $(printf %q "$pack") | ssh -o StrictHostKeyChecking=accept-new $b_login@$b_addr $(printf %q "sudo -n bash -c $(printf %q "$unpack")")"
  fi
}

cmd_run() {
  local a=$1 b=$2 w=$3 tmp ck rs_ verdict detail t0 t1
  tmp=$(mktemp -d)
  log "$w: $a -> $b"

  local rc=0
  on "$a" "$REMOTE/run.sh prepare $w && $REMOTE/run.sh start $w && $REMOTE/run.sh warm $w" || rc=$?
  if (( rc == 75 )); then
    log "$w: skipped on $a (not enough memory)"
    jq -n -c --arg w "$w" --arg a "$a" '{workload:$w, mode:"cross", verdict:"skip", detail:("not enough memory on " + $a)}' \
      | tee -a "$SPIKE_DIR/results/runs.jsonl"
    rm -rf "$tmp"
    return 0
  fi
  (( rc == 0 )) || { rm -rf "$tmp"; return "$rc"; }
  on "$a" "$REMOTE/run.sh probe $w" > "$tmp/before.json"
  ck=$(on "$a" "$REMOTE/run.sh checkpoint $w")
  log "$w: checkpoint on $a: $(jq -c . <<<"$ck")"

  t0=$(now_ms)
  move "$a" "$b" "$w"
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

  mkdir -p "$SPIKE_DIR/results"
  jq -n -c \
    --arg w "$w" --arg verdict "$verdict" --arg detail "$detail" --arg mode cross --arg transfer "$TRANSFER" \
    --argjson from "$(on "$a" "$REMOTE/run.sh host-info" | jq -c .)" \
    --argjson to "$(on "$b" "$REMOTE/run.sh host-info" | jq -c .)" \
    --argjson ck "$ck" --argjson rs "$rs_" --argjson transfer_ms $((t1 - t0)) \
    '{workload:$w, mode:$mode, verdict:$verdict, detail:$detail, from:$from, to:$to, transfer:$transfer, transfer_ms:$transfer_ms} + $ck + $rs' \
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
  *) sed -n '2,20p' "$0"; exit 2 ;;
esac
