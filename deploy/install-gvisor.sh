#!/usr/bin/env bash
# Install gVisor as pinned in deploy/versions.env, each release verified against
# its published checksum: RUNSC_RELEASE as the default runsc, plus every release
# in RUNSC_KEEP next to it. Used by host-setup.sh, CI and the container image,
# so they all run the same runsc. Idempotent.
#
# One folder per release, kept side by side: a full save (and so a paused
# workspace) restores only with the release that made it, and the engine finds
# that release under /opt/gvisor. When upgrading, move the old RUNSC_RELEASE
# into RUNSC_KEEP. runsc finds its helper binaries next to its real path, so it
# is symlinked, not copied.
#
#   sudo deploy/install-gvisor.sh             # as pinned
#   sudo deploy/install-gvisor.sh 20260928    # one extra release, default unchanged
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=versions.env
source "$HERE/versions.env"

log() { echo "[install-gvisor] $*" >&2; }
[[ $EUID -eq 0 ]] || { log "run as root (sudo)"; exit 1; }

case $(uname -m) in
  x86_64) arch=x86_64 ;;
  aarch64 | arm64) arch=aarch64 ;;
  *) log "unsupported architecture $(uname -m)"; exit 1 ;;
esac

install() {
  local release=$1 dir=/opt/gvisor/$1 tmp
  if [[ ! -x $dir/runsc ]]; then
    log "gVisor $release ($arch)"
    local url=https://storage.googleapis.com/gvisor/releases/release/$release/$arch/gvisor.tar.zstd
    tmp=$(mktemp -d)
    curl -fsSL --retry 3 -o "$tmp/gvisor.tar.zstd" "$url"
    curl -fsSL --retry 3 -o "$tmp/gvisor.tar.zstd.sha512" "$url.sha512"
    (cd "$tmp" && sha512sum -c gvisor.tar.zstd.sha512 >/dev/null) || { log "gVisor $release checksum mismatch"; rm -rf "$tmp"; exit 1; }
    mkdir -p "$dir"
    zstd -dc "$tmp/gvisor.tar.zstd" | tar -x -C "$dir"
    rm -rf "$tmp"
  fi
  "$dir/runsc" --version | head -1
}

if [[ $# -gt 0 ]]; then
  install "$1"
  exit 0
fi
for r in ${RUNSC_KEEP:-}; do install "$r"; done
install "$RUNSC_RELEASE"
ln -sfn "/opt/gvisor/$RUNSC_RELEASE/runsc" /usr/local/bin/runsc
ln -sfn "/opt/gvisor/$RUNSC_RELEASE/containerd-shim-runsc-v1" /usr/local/bin/containerd-shim-runsc-v1
