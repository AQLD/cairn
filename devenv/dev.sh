#!/usr/bin/env bash
# One entry point for Cairn's Linux environments.
#
# Local (free): two Ubuntu 24.04 VMs, cairn-a and cairn-b, set up exactly like
# the droplets by deploy/host-setup.sh, with this repo mounted at the same path.
#   dev.sh up                    create/start both VMs and run host setup
#   dev.sh shell [VM]            shell in the repo on a VM (default cairn-a)
#   dev.sh test [ARGS...]        go test on Linux (default: -short ./...)
#   dev.sh spike [WORKLOAD]      same-host save/restore (all workloads by default)
#   dev.sh cross [WORKLOAD]      save on cairn-a, restore on cairn-b (and back)
#   dev.sh status | stop | delete
#
# DigitalOcean (billed hourly): run everything queued in one sitting, then delete.
#   dev.sh do-up SSH_KEY_ID [REGION]     create the two droplets (asks first)
#   dev.sh do-session A B                setup, spike (same-host + cross-CPU), tests, benchmarks
#   dev.sh do-down                       delete the droplets (asks first)
set -euo pipefail

DEVENV=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$DEVENV/.." && pwd)
VMS=(cairn-a cairn-b)
SSH_CONFIG=$DEVENV/.ssh.config
RESULTS=$REPO/spike/results

LIMACTL=$(command -v limactl || echo "$HOME/.local/lima/bin/limactl")
[[ -x $LIMACTL ]] || LIMACTL=""

log() { echo "[dev] $*" >&2; }
die() { log "error: $*"; exit 1; }
need_lima() { [[ -n $LIMACTL ]] || die "Lima not found: see devenv/README.md"; }
vm_state() { "$LIMACTL" list --format '{{.Status}}' "$1" 2>/dev/null || true; }
in_vm() { local vm=$1; shift; "$LIMACTL" shell --workdir "$REPO" "$vm" -- bash -lc "$*"; }

write_ssh_config() {
  : > "$SSH_CONFIG"
  for vm in "${VMS[@]}"; do cat "$HOME/.lima/$vm/ssh.config" >> "$SSH_CONFIG"; done
}

cmd_up() {
  need_lima
  for vm in "${VMS[@]}"; do
    if [[ -z $(vm_state "$vm") ]]; then
      log "$vm: creating (first run downloads the Ubuntu image)"
      "$LIMACTL" create --tty=false --name "$vm" \
        --set ".mounts = [{\"location\": \"$REPO\", \"writable\": true}]" \
        "$DEVENV/lima.yaml" >/dev/null
    fi
    if [[ $(vm_state "$vm") != Running ]]; then
      log "$vm: starting"
      "$LIMACTL" start --tty=false "$vm" >/dev/null
    fi
    log "$vm: host setup"
    in_vm "$vm" "sudo -n bash deploy/host-setup.sh" > "$DEVENV/.$vm.info.json"
    log "$vm: $(jq -c '{arch, os, runsc, go}' "$DEVENV/.$vm.info.json")"
  done
  write_ssh_config
  log "ready. try: devenv/dev.sh test   or   devenv/dev.sh spike"
}

cmd_shell() { need_lima; "$LIMACTL" shell --workdir "$REPO" "${1:-cairn-a}"; }

cmd_test() {
  need_lima
  local args=${*:-"-short ./..."}
  in_vm cairn-a "go test $args"
}

cmd_spike() {
  need_lima
  if [[ $# -eq 0 ]]; then in_vm cairn-a "sudo -n spike/run.sh local-all"
  else in_vm cairn-a "sudo -n spike/run.sh local $1"; fi
}

# Local cross-host run: the repo is mounted on both VMs, saves are relayed
# through the Mac. Same script and steps as on the droplets.
cmd_cross() {
  need_lima
  write_ssh_config
  export CAIRN_SSH_CONFIG=$SSH_CONFIG CAIRN_REMOTE_REPO=$REPO CAIRN_SYNC=none CAIRN_TRANSFER=relay
  if [[ $# -eq 0 ]]; then "$REPO/spike/cross.sh" all lima-cairn-a lima-cairn-b
  else
    "$REPO/spike/cross.sh" run lima-cairn-a lima-cairn-b "$1"
    "$REPO/spike/cross.sh" run lima-cairn-b lima-cairn-a "$1"
  fi
}

cmd_status() { need_lima; "$LIMACTL" list; }
cmd_stop() { need_lima; for vm in "${VMS[@]}"; do "$LIMACTL" stop "$vm" || true; done; }
cmd_delete() {
  need_lima
  read -r -p "Delete the dev VMs ${VMS[*]} and their disks? [y/N] " ok
  [[ $ok == y || $ok == Y ]] || exit 1
  for vm in "${VMS[@]}"; do "$LIMACTL" delete --force "$vm" || true; done
  rm -f "$SSH_CONFIG" "$DEVENV"/.cairn-*.info.json
}

# --- DigitalOcean --------------------------------------------------------------

cmd_do_up() { "$REPO/deploy/do-provision.sh" "$@"; }
cmd_do_down() { "$REPO/deploy/do-destroy.sh"; }

# Everything worth running on real hardware, in one sitting. Each step logs to
# spike/results/<session>/ and a failing step does not stop the rest.
cmd_do_session() {
  [[ $# -eq 2 ]] || die "usage: dev.sh do-session A B   (e.g. root@<intel-ip> root@<amd-ip>)"
  local a=$1 b=$2 dir status=0
  dir=$RESULTS/session-$(date +%Y%m%d-%H%M%S)
  mkdir -p "$dir"
  export CAIRN_REMOTE_REPO=/root/cairn CAIRN_SYNC=rsync CAIRN_TRANSFER=direct
  step() {
    local name=$1; shift
    log "session: $name"
    if "$@" > "$dir/$name.log" 2>&1; then log "  ok"; else log "  FAILED (see $dir/$name.log)"; status=1; fi
  }
  on() { ssh -o BatchMode=yes "$1" "sudo -n bash -c $(printf %q "$2")"; }

  step sync "$REPO/spike/cross.sh" sync "$a" "$b"
  step host-info-a on "$a" "/root/cairn/deploy/host-setup.sh --info"
  step host-info-b on "$b" "/root/cairn/deploy/host-setup.sh --info"
  step spike-local-a on "$a" "/root/cairn/spike/run.sh local-all"
  step spike-local-b on "$b" "/root/cairn/spike/run.sh local-all"
  step spike-cross "$REPO/spike/cross.sh" all "$a" "$b"
  step go-test-a on "$a" "cd /root/cairn && /usr/local/go/bin/go test -count=1 ./..."
  step bench-a on "$a" "cd /root/cairn && /usr/local/go/bin/go test -run XXX -bench . -benchmem ./bench"
  step bench-b on "$b" "cd /root/cairn && /usr/local/go/bin/go test -run XXX -bench . -benchmem ./bench"
  cp "$RESULTS/runs.jsonl" "$dir/" 2>/dev/null || true

  log "session done: $dir"
  log "the droplets are still running and billing: devenv/dev.sh do-down"
  return $status
}

cmd=${1:-}; shift || true
case $cmd in
  up | shell | test | spike | cross | status | stop | delete) "cmd_$cmd" "$@" ;;
  do-up) cmd_do_up "$@" ;;
  do-session) cmd_do_session "$@" ;;
  do-down) cmd_do_down ;;
  *) sed -n '2,20p' "$0"; exit 2 ;;
esac
