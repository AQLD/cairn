#!/bin/sh
# Installs the cairn CLI from the latest GitHub release (or CAIRN_VERSION),
# checking its SHA-256 against the release's SHA256SUMS.
#
#   curl -fsSL https://raw.githubusercontent.com/AQLD/cairn/main/install.sh | sh
#
# Then: `cairn up` (an engine in Docker) and `cairn setup --project` in a
# repository (connects Claude Code, Codex, pi and Cursor).
#
# CAIRN_VERSION   a release tag such as v0.1.0 (default: the latest release)
# CAIRN_INSTALL   where to put cairn (default: /usr/local/bin if writable,
#                 else ~/.local/bin)
# CAIRN_DOWNLOAD_BASE  where release files are (default: the GitHub release
#                 for the version; a mirror for offline networks)
set -eu

repo=AQLD/cairn
say() { printf '%s\n' "$*" >&2; }
die() { say "cairn install: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "needs $1"; }
need curl
need tar
need uname

case $(uname -s) in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) die "unsupported system $(uname -s) (macOS and Linux only; on Windows use WSL)" ;;
esac
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported CPU $(uname -m)" ;;
esac

version=${CAIRN_VERSION:-}
if [ -z "$version" ] && [ -n "${CAIRN_DOWNLOAD_BASE:-}" ]; then
	die "CAIRN_DOWNLOAD_BASE needs CAIRN_VERSION"
fi
if [ -z "$version" ]; then
	# the redirect of /releases/latest names the newest release's tag
	version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" | sed 's#.*/tag/##')
	case $version in
	v*) ;;
	*) die "no release found yet; with Go installed: go install github.com/$repo/cmd/cairn@latest" ;;
	esac
fi

name="cairn_${version}_${os}_${arch}"
base=${CAIRN_DOWNLOAD_BASE:-"https://github.com/$repo/releases/download/$version"}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "downloading cairn $version for $os/$arch"
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz" || die "could not download $base/$name.tar.gz"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "could not download SHA256SUMS"

want=$(grep " $name.tar.gz\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)
[ -n "$want" ] || die "$name.tar.gz is not in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)
else
	got=$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)
fi
[ "$got" = "$want" ] || die "checksum mismatch for $name.tar.gz (got $got, want $want)"

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

dest=${CAIRN_INSTALL:-}
if [ -z "$dest" ]; then
	if [ -w /usr/local/bin ]; then dest=/usr/local/bin; else dest="$HOME/.local/bin"; fi
fi
mkdir -p "$dest"
# replace atomically, so a running cairn (an agent's MCP server) is not broken
cp "$tmp/$name/cairn" "$dest/.cairn.new"
chmod 755 "$dest/.cairn.new"
mv -f "$dest/.cairn.new" "$dest/cairn"
say "installed $dest/cairn ($("$dest/cairn" version 2>/dev/null || echo "$version"))"

case ":$PATH:" in
*":$dest:"*) ;;
*) say "note: $dest is not on your PATH; add it, e.g. export PATH=\"$dest:\$PATH\"" ;;
esac

say ""
say "next:"
say "  cairn up                    # a local engine in Docker (gVisor workspaces)"
say "  cd your-project && cairn setup --project   # connect Claude Code, Codex, pi, Cursor"
