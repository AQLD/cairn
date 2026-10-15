#!/usr/bin/env bash
# E0 spike: drive gVisor's runsc directly to save and restore real workloads.
# Runs on a Linux host as root. See spike/README.md.
#
#   run.sh setup                   prepare the host (deploy/host-setup.sh) and spike folders
#   run.sh host-info               CPU, kernel and runsc version as JSON
#   run.sh local WORKLOAD          full cycle on this host: start, warm up, checkpoint, restore, verify
#   run.sh local-all               every workload
#   run.sh compat [TEST...]        run the compatibility tests under gVisor and, as a
#                                  control, under plain Docker (runc); see spike/compat/
#
# Building blocks (used by cross.sh to move a workload between hosts):
#   run.sh prepare WORKLOAD        build image, bundle and network namespace
#   run.sh start|probe|warm|checkpoint|restore|stop WORKLOAD
#   run.sh exec WORKLOAD COMMAND...   run a command inside the sandbox
#   run.sh check WORKLOAD BEFORE.json AFTER.json
set -euo pipefail

SPIKE_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
WORK=${CAIRN_SPIKE_WORK:-/var/lib/cairn-spike}
RUNSC=${RUNSC:-runsc}
RESULTS=${CAIRN_SPIKE_RESULTS:-$SPIKE_DIR/results}

# Global runsc flags. They must be identical for checkpoint and restore.
#  - overlay2 root:memory: the workspace's writes live in sandbox memory, so the
#    checkpoint image carries them; the bundle's rootfs is the unchanged lower layer.
RUNSC_FLAGS=(--root "$WORK/runsc-state" --overlay2=root:memory)

rs() { "$RUNSC" "${RUNSC_FLAGS[@]}" "$@"; }
log() { echo "[$(date +%H:%M:%S)] $*" >&2; }
die() { log "error: $*"; exit 1; }
now_ms() { date +%s%3N; }

workloads() { ls "$SPIKE_DIR/workloads"; }
wdir() { local d="$SPIKE_DIR/workloads/$1"; [[ -d $d ]] || die "unknown workload $1 (have: $(workloads | tr '\n' ' '))"; echo "$d"; }
bundle() { echo "$WORK/bundles/$1"; }
image() { echo "$WORK/images/$1"; }
cid() { echo "spike-$1"; }

# Each workload's workload.conf sets IDX (network slot), WARM_TIMEOUT and SETTLE,
# and optionally:
#   BUILD_DIR    image to build, relative to spike/ (default: the workload's own folder)
#   ENV          space-separated KEY=VALUE pairs added to the workload's environment
#   MIN_FREE_MB  skip (not fail) on hosts with less available memory than this
load_conf() {
  IDX=0 WARM_TIMEOUT=60 SETTLE=3 BUILD_DIR="" ENV="" MIN_FREE_MB=0
  source "$(wdir "$1")/workload.conf"
  [[ -n $BUILD_DIR ]] && BUILD_DIR="$SPIKE_DIR/$BUILD_DIR" || BUILD_DIR=$(wdir "$1")
}

free_mb() { awk '/MemAvailable/ {print int($2/1024)}' /proc/meminfo; }

# --- setup -------------------------------------------------------------------

# Host setup is shared with every Cairn host: deploy/host-setup.sh (pinned in deploy/versions.env).
cmd_setup() {
  [[ $EUID -eq 0 ]] || die "run as root"
  bash "$SPIKE_DIR/../deploy/host-setup.sh" >/dev/null
  mkdir -p "$WORK"/{bundles,images,runsc-state,logs} "$RESULTS"
  log "ready: $(runsc --version | head -1)"
}

cmd_host_info() {
  bash "$SPIKE_DIR/../deploy/host-setup.sh" --info
}

# --- network: one namespace per workload, NAT to the internet ----------------
# runsc's user-space network stack takes its addresses and routes from this
# namespace. The target host of a restore must recreate it identically.

# runsc moves the namespace's address and routes into its user-space network
# stack, leaving the namespace bare, so this re-applies the full configuration
# before every start and every restore instead of only on first creation.
net_up() {
  local i=$IDX ns="cs-$1" host_if="cs$IDX-h" ws_if="cs$IDX-w"
  ip netns add "$ns" 2>/dev/null || true
  if ! ip -n "$ns" link show "$ws_if" >/dev/null 2>&1; then
    ip link del "$host_if" 2>/dev/null || true
    ip link add "$host_if" type veth peer name "$ws_if"
    ip link set "$ws_if" netns "$ns"
  fi
  ip addr replace "10.200.$i.1/24" dev "$host_if"
  ip link set "$host_if" up
  ip -n "$ns" addr flush dev "$ws_if"
  ip -n "$ns" addr add "10.200.$i.2/24" dev "$ws_if"
  ip -n "$ns" link set "$ws_if" up
  ip -n "$ns" link set lo up
  ip -n "$ns" route replace default via "10.200.$i.1"
  sysctl -qw net.ipv4.ip_forward=1
  iptables -t nat -C POSTROUTING -s "10.200.$i.0/24" ! -o "$host_if" -j MASQUERADE 2>/dev/null \
    || iptables -t nat -A POSTROUTING -s "10.200.$i.0/24" ! -o "$host_if" -j MASQUERADE
  # docker sets the FORWARD policy to DROP; let this workload's traffic through
  iptables -C FORWARD -i "$host_if" -j ACCEPT 2>/dev/null || iptables -I FORWARD -i "$host_if" -j ACCEPT
  iptables -C FORWARD -o "$host_if" -j ACCEPT 2>/dev/null || iptables -I FORWARD -o "$host_if" -j ACCEPT
}

net_down() {
  ip link del "cs$IDX-h" 2>/dev/null || true
  ip netns del "cs-$1" 2>/dev/null || true
}

# --- bundle ------------------------------------------------------------------

cmd_prepare() {
  local w=$1 b dir
  load_conf "$w"
  dir=$BUILD_DIR; b=$(bundle "$w")
  log "$w: building image"
  docker build -q -t "cairn-spike/$w" "$dir" >/dev/null
  log "$w: exporting root filesystem"
  rm -rf "$b"; mkdir -p "$b/rootfs"
  local c; c=$(docker create "cairn-spike/$w")
  docker export "$c" | tar -x -C "$b/rootfs"
  docker rm -f "$c" >/dev/null
  echo "nameserver 1.1.1.1" > "$b/rootfs/etc/resolv.conf"
  ( cd "$b" && rs spec -- /spike/start.sh )
  local tmp; tmp=$(mktemp)
  jq --arg ns "/var/run/netns/cs-$w" --arg host "$(cid "$w")" --arg env "$ENV" '
      .process.env += ($env | split(" ") | map(select(length > 0)))
    | .process.terminal = false
    | .process.cwd = "/"
    | .root.readonly = false
    | .hostname = $host
    | .linux.namespaces = ([.linux.namespaces[] | select(.type != "network")] + [{type: "network", path: $ns}])
  ' "$b/config.json" > "$tmp" && mv "$tmp" "$b/config.json"
  net_up "$w"
  log "$w: bundle ready at $b"
}

# --- lifecycle ---------------------------------------------------------------

# Exit code for "this host cannot run the workload" (too little memory): callers
# record a skip, not a failure.
EX_SKIP=75

cmd_start() {
  local w=$1 id; id=$(cid "$w"); load_conf "$w"
  if (( MIN_FREE_MB > 0 && $(free_mb) < MIN_FREE_MB )); then
    log "$w: needs ${MIN_FREE_MB} MB free, host has $(free_mb) MB"
    exit $EX_SKIP
  fi
  net_up "$w"
  rs delete --force "$id" >/dev/null 2>&1 || true
  rs run --detach --bundle "$(bundle "$w")" "$id" </dev/null >"$WORK/logs/$w.out" 2>&1
  log "$w: started"
}

cmd_probe() {
  local w=$1
  rs exec "$(cid "$w")" /spike/probe 2>/dev/null
}

# Wait until the workload is in the state we want to checkpoint it in.
cmd_warm() {
  local w=$1 deadline; load_conf "$w"
  deadline=$(( $(date +%s) + WARM_TIMEOUT ))
  until cmd_probe "$w" | jq -e '.ready == true' >/dev/null 2>&1; do
    (( $(date +%s) < deadline )) || die "$w: not ready after ${WARM_TIMEOUT}s: $(cmd_probe "$w" || true)"
    sleep 1
  done
  log "$w: warm"
}

# Prints {"checkpoint_ms": N, "image_bytes": N}. The sandbox exits afterwards.
cmd_checkpoint() {
  local w=$1 img t0 t1; img=$(image "$w")
  rm -rf "$img"; mkdir -p "$img"
  t0=$(now_ms)
  rs checkpoint --image-path "$img" "$(cid "$w")" >>"$WORK/logs/$w.out" 2>&1
  t1=$(now_ms)
  rs delete --force "$(cid "$w")" >/dev/null 2>&1 || true
  jq -n --argjson ms $((t1 - t0)) --argjson bytes "$(du -sb "$img" | cut -f1)" '{checkpoint_ms:$ms, image_bytes:$bytes}'
}

# Prints {"restore_ms": N}: time until the restored sandbox answers a probe.
cmd_restore() {
  local w=$1 t0 t1; load_conf "$w"
  net_up "$w"
  t0=$(now_ms)
  rs restore --detach --image-path "$(image "$w")" --bundle "$(bundle "$w")" "$(cid "$w")" \
    </dev/null >>"$WORK/logs/$w.out" 2>&1
  local tries=0
  until cmd_probe "$w" >/dev/null 2>&1; do
    (( ++tries < 300 )) || die "$w: restored sandbox does not answer"
    sleep 0.1
  done
  t1=$(now_ms)
  jq -n --argjson ms $((t1 - t0)) '{restore_ms:$ms}'
}

cmd_exec() {
  local w=$1; shift
  rs exec "$(cid "$w")" "$@"
}

cmd_stop() {
  local w=$1; load_conf "$w"
  rs kill "$(cid "$w")" KILL >/dev/null 2>&1 || true
  rs delete --force "$(cid "$w")" >/dev/null 2>&1 || true
  net_down "$w"
}

cmd_check() {
  local w=$1 before=$2 after=$3
  bash "$(wdir "$w")/check.sh" "$before" "$after"
}

# --- full cycle on one host --------------------------------------------------

cmd_local() {
  local w=$1 out ck rs_ verdict detail
  out="$WORK/runs/$w"
  load_conf "$w"
  if (( MIN_FREE_MB > 0 && $(free_mb) < MIN_FREE_MB )); then
    log "$w: skipped, needs ${MIN_FREE_MB} MB free, host has $(free_mb) MB"
    jq -n -c --arg w "$w" --arg detail "needs ${MIN_FREE_MB} MB free memory, host has $(free_mb) MB" \
      --argjson host "$(cmd_host_info | jq -c .)" \
      '{workload:$w, mode:"local", verdict:"skip", detail:$detail, host:$host}' | tee -a "$RESULTS/runs.jsonl"
    return 0
  fi
  mkdir -p "$out"
  cmd_prepare "$w"
  cmd_start "$w"
  cmd_warm "$w"
  cmd_probe "$w" > "$out/before.json"
  ck=$(cmd_checkpoint "$w")
  log "$w: checkpointed $(echo "$ck" | jq -c .)"
  rs_=$(cmd_restore "$w")
  log "$w: restored $(echo "$rs_" | jq -c .)"
  sleep "$SETTLE"
  cmd_probe "$w" > "$out/after.json" || echo '{}' > "$out/after.json"
  if detail=$(WORKLOAD="$w" RUN="$SPIKE_DIR/run.sh" cmd_check "$w" "$out/before.json" "$out/after.json" 2>&1); then
    verdict=pass
  else
    verdict=fail
  fi
  cmd_stop "$w"
  jq -n -c \
    --arg w "$w" --arg verdict "$verdict" --arg detail "$detail" --arg mode local \
    --argjson host "$(cmd_host_info | jq -c .)" \
    --argjson ck "$ck" --argjson rs "$rs_" \
    '{workload:$w, mode:$mode, verdict:$verdict, detail:$detail, host:$host} + $ck + $rs' \
    | tee -a "$RESULTS/runs.jsonl"
  [[ $verdict == pass ]]
}

cmd_local_all() {
  local failed=0
  for w in $(workloads); do cmd_local "$w" || failed=1; done
  return $failed
}

# --- compatibility list -------------------------------------------------------
# Each test in spike/compat/tests runs twice: in a gVisor sandbox and, as a
# control, in an ordinary Docker container. Only "gvisor-only" failures belong
# on the compatibility list; failures in both are the network or the test.

cmd_compat() {
  local name=compat img=cairn-spike/compat b rootfs base t tb out ec ms rc rms verdict tests
  IDX=9
  b=$(bundle "$name")
  log "compat: building toolbox image"
  docker build -q -t "$img" "$SPIKE_DIR/compat" >/dev/null
  rm -rf "$b"; mkdir -p "$b/rootfs"
  local c; c=$(docker create "$img")
  docker export "$c" | tar -x -C "$b/rootfs"
  docker rm -f "$c" >/dev/null
  echo "nameserver 1.1.1.1" > "$b/rootfs/etc/resolv.conf"
  ( cd "$b" && rs spec -- /bin/true )
  rootfs="$b/rootfs"
  base=$(jq --arg ns "/var/run/netns/cs-$name" --arg root "$rootfs" '
      .process.terminal = false | .process.cwd = "/tmp" | .root.path = $root | .root.readonly = false
    | .hostname = "compat"
    | .linux.namespaces = ([.linux.namespaces[] | select(.type != "network")] + [{type: "network", path: $ns}])' "$b/config.json")
  net_up "$name"

  if [[ $# -gt 0 ]]; then tests=("$@"); else tests=($(cd "$SPIKE_DIR/compat/tests" && ls | sed 's/\.sh$//')); fi
  local pass=0 gvisor_only=0 other=0
  for t in "${tests[@]}"; do
    tb="$WORK/compat-bundles/$t"; out="$WORK/logs/compat-$t"
    mkdir -p "$tb"
    jq --arg t "/compat/tests/$t.sh" '.process.args = [$t]' <<<"$base" > "$tb/config.json"
    rs delete --force "compat-$t" >/dev/null 2>&1 || true
    local t0; t0=$(now_ms)
    timeout 300 "$RUNSC" "${RUNSC_FLAGS[@]}" run --bundle "$tb" "compat-$t" </dev/null >"$out.runsc" 2>&1 && ec=0 || ec=$?
    ms=$(( $(now_ms) - t0 ))
    rs delete --force "compat-$t" >/dev/null 2>&1 || true
    net_up "$name" # runsc strips the namespace; re-apply for the next test
    t0=$(now_ms)
    timeout 300 docker run --rm "$img" "/compat/tests/$t.sh" </dev/null >"$out.runc" 2>&1 && rc=0 || rc=$?
    rms=$(( $(now_ms) - t0 ))
    if (( ec == 0 )); then verdict=ok; pass=$((pass + 1))
    elif (( rc == 0 )); then verdict=gvisor-only-failure; gvisor_only=$((gvisor_only + 1))
    else verdict=fails-everywhere; other=$((other + 1)); fi
    log "compat: $t: $verdict (gvisor exit $ec in ${ms} ms, docker exit $rc in ${rms} ms)"
    jq -n -c --arg t "$t" --arg v "$verdict" --argjson ec "$ec" --argjson ms "$ms" --argjson rc "$rc" --argjson rms "$rms" \
      --arg about "$(sed -n '2s/^# //p' "$SPIKE_DIR/compat/tests/$t.sh")" \
      --arg tail "$(tail -c 600 "$out.runsc")" --argjson host "$(cmd_host_info | jq -c .)" \
      '{mode:"compat", test:$t, about:$about, verdict:$v, gvisor:{exit:$ec, ms:$ms}, docker:{exit:$rc, ms:$rms}, gvisor_output_tail:$tail, host:$host}' \
      >> "$RESULTS/compat.jsonl"
  done
  net_down "$name"
  log "compat: $pass ok, $gvisor_only gVisor-only failures, $other failing everywhere (details: $RESULTS/compat.jsonl)"
  (( gvisor_only == 0 ))
}

main() {
  local cmd=${1:-}; shift || true
  [[ $EUID -ne 0 ]] || mkdir -p "$WORK"/{bundles,images,runsc-state,logs,runs} "$RESULTS"
  case $cmd in
    setup) cmd_setup ;;
    host-info) cmd_host_info ;;
    prepare|start|probe|warm|checkpoint|restore|stop|local) [[ $# -ge 1 ]] || die "usage: $0 $cmd WORKLOAD"; "cmd_$cmd" "$@" ;;
    exec) [[ $# -ge 2 ]] || die "usage: $0 exec WORKLOAD COMMAND..."; cmd_exec "$@" ;;
    check) [[ $# -eq 3 ]] || die "usage: $0 check WORKLOAD BEFORE AFTER"; cmd_check "$@" ;;
    local-all) cmd_local_all ;;
    compat) cmd_compat "$@" ;;
    *) sed -n '2,15p' "$0"; exit 2 ;;
  esac
}

main "$@"
