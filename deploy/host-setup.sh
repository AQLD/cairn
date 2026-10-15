#!/usr/bin/env bash
# Prepare a Linux host to run Cairn: the same script for local dev VMs, the
# DigitalOcean test droplets and production, so they never drift apart.
# Idempotent: safe to run again after changing deploy/versions.env.
#
#   sudo deploy/host-setup.sh        then: deploy/host-setup.sh --info
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=versions.env
source "$HERE/versions.env"

log() { echo "[host-setup] $*" >&2; }

arch() {
  case $(uname -m) in
    x86_64) echo "x86_64 amd64" ;;
    aarch64 | arm64) echo "aarch64 arm64" ;;
    *) log "unsupported architecture $(uname -m)"; exit 1 ;;
  esac
}

info() {
  # One JSON object describing the host, recorded with every test run.
  local flags
  flags=$(grep -m1 -E '^(flags|Features)' /proc/cpuinfo | cut -d: -f2 | tr ' ' '\n' | sed '/^$/d' | sort | tr '\n' ' ')
  jq -n \
    --arg host "$(hostname)" \
    --arg arch "$(uname -m)" \
    --arg cpu "$(grep -m1 -E 'model name|CPU part' /proc/cpuinfo | cut -d: -f2 | xargs)" \
    --arg cpus "$(nproc)" \
    --arg kernel "$(uname -r)" \
    --arg os "$(. /etc/os-release && echo "$PRETTY_NAME")" \
    --arg runsc "$(runsc --version 2>/dev/null | head -1)" \
    --arg go "$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}')" \
    --arg flags_sha "$(printf %s "$flags" | sha256sum | cut -c1-16)" \
    '{host:$host, arch:$arch, cpu:$cpu, cpus:($cpus|tonumber), kernel:$kernel, os:$os, runsc:$runsc, go:$go, cpu_flags_sha:$flags_sha}'
}

if [[ ${1:-} == --info ]]; then info; exit 0; fi
[[ $EUID -eq 0 ]] || { log "run as root (sudo)"; exit 1; }

read -r GV_ARCH GO_ARCH <<<"$(arch)"
. /etc/os-release
[[ $VERSION_ID == "$UBUNTU_RELEASE" ]] || log "warning: Ubuntu $VERSION_ID, pinned $UBUNTU_RELEASE"

log "packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  ca-certificates curl jq zstd git make iproute2 iptables rsync docker.io >/dev/null
systemctl enable --now docker >/dev/null 2>&1 || true

# gVisor: one folder per release, kept side by side so a save made with an older
# release can still be restored by it (upgrade-safe restore). runsc finds its
# helper binaries (gvisor-bin/) next to its real path, so it is symlinked, not copied.
gv_dir=/opt/gvisor/$RUNSC_RELEASE
if [[ ! -x $gv_dir/runsc ]]; then
  log "gVisor $RUNSC_RELEASE ($GV_ARCH)"
  url=https://storage.googleapis.com/gvisor/releases/release/$RUNSC_RELEASE/$GV_ARCH/gvisor.tar.zstd
  tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/gvisor.tar.zstd" "$url"
  curl -fsSL -o "$tmp/gvisor.tar.zstd.sha512" "$url.sha512"
  (cd "$tmp" && sha512sum -c gvisor.tar.zstd.sha512 >/dev/null) || { log "gVisor checksum mismatch"; exit 1; }
  mkdir -p "$gv_dir"
  zstd -dc "$tmp/gvisor.tar.zstd" | tar -x -C "$gv_dir"
  rm -rf "$tmp"
fi
ln -sfn "$gv_dir/runsc" /usr/local/bin/runsc
ln -sfn "$gv_dir/containerd-shim-runsc-v1" /usr/local/bin/containerd-shim-runsc-v1

# Go, for building and testing cairn on the host.
if [[ $(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}') != "go$GO_VERSION" ]]; then
  log "Go $GO_VERSION ($GO_ARCH)"
  rm -rf /usr/local/go
  curl -fsSL "https://go.dev/dl/go$GO_VERSION.linux-$GO_ARCH.tar.gz" | tar -xz -C /usr/local
fi
echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh

# Kernel settings the engine relies on.
cat > /etc/sysctl.d/90-cairn.conf <<'EOF'
# workspaces reach the network through NAT from their own namespaces
net.ipv4.ip_forward = 1
EOF
sysctl -q --system

mkdir -p /var/lib/cairn /var/lib/cairn-spike
log "done"
info
