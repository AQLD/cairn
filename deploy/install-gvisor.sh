#!/usr/bin/env bash
# Install the pinned gVisor release (deploy/versions.env), verified against its
# published checksum. Used by host-setup.sh and by CI, so both run the same
# runsc. Idempotent.
#
# One folder per release, kept side by side so a save made with an older
# release can still be restored by it (upgrade-safe restore). runsc finds its
# helper binaries next to its real path, so it is symlinked, not copied.
#
#   sudo deploy/install-gvisor.sh
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

dir=/opt/gvisor/$RUNSC_RELEASE
if [[ ! -x $dir/runsc ]]; then
  log "gVisor $RUNSC_RELEASE ($arch)"
  url=https://storage.googleapis.com/gvisor/releases/release/$RUNSC_RELEASE/$arch/gvisor.tar.zstd
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL --retry 3 -o "$tmp/gvisor.tar.zstd" "$url"
  curl -fsSL --retry 3 -o "$tmp/gvisor.tar.zstd.sha512" "$url.sha512"
  (cd "$tmp" && sha512sum -c gvisor.tar.zstd.sha512 >/dev/null) || { log "gVisor checksum mismatch"; exit 1; }
  mkdir -p "$dir"
  zstd -dc "$tmp/gvisor.tar.zstd" | tar -x -C "$dir"
fi
ln -sfn "$dir/runsc" /usr/local/bin/runsc
ln -sfn "$dir/containerd-shim-runsc-v1" /usr/local/bin/containerd-shim-runsc-v1
runsc --version | head -1
