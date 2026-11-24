#!/bin/sh
# Installs the cairn CLI from the latest GitHub release (or CAIRN_VERSION),
# checking its SHA-256 against the release's SHA256SUMS, and SHA256SUMS's
# Sigstore signature when cosign is installed.
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
# CAIRN_REQUIRE_SIGNATURE=1  refuse to install without checking the signature
# CAIRN_COSIGN_KEY  a public key to check the signature with, instead of the
#                 release workflow's identity (a mirror that signs itself)
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

# SHA256SUMS is what everything else is checked against: check it first
signed=1
curl -fsSL -o "$tmp/SHA256SUMS.sigstore.json" "$base/SHA256SUMS.sigstore.json" 2>/dev/null || signed=0
if [ "$signed" = 0 ]; then
	# only releases from before signing began may lack a signature: for any
	# other, a missing one is an attack (stripping it to skip the check)
	case $version in
	v0.1.0 | v0.2.0) ;;
	*) if command -v cosign >/dev/null 2>&1 || [ "${CAIRN_REQUIRE_SIGNATURE:-}" = 1 ]; then
		die "$version has no signature (SHA256SUMS.sigstore.json); refusing to install it"
	fi ;;
	esac
	[ "${CAIRN_REQUIRE_SIGNATURE:-}" = 1 ] && die "$version is not signed, and CAIRN_REQUIRE_SIGNATURE=1"
	say "note: $version is not signed; the download is checked against SHA256SUMS"
elif command -v cosign >/dev/null 2>&1; then
	if [ -n "${CAIRN_COSIGN_KEY:-}" ]; then
		cosign verify-blob --key "$CAIRN_COSIGN_KEY" --bundle "$tmp/SHA256SUMS.sigstore.json" "$tmp/SHA256SUMS" >/dev/null 2>&1 ||
			die "SHA256SUMS signature does not verify with $CAIRN_COSIGN_KEY"
	else
		cosign verify-blob --bundle "$tmp/SHA256SUMS.sigstore.json" \
			--certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$version" \
			--certificate-oidc-issuer https://token.actions.githubusercontent.com "$tmp/SHA256SUMS" >/dev/null 2>&1 ||
			die "SHA256SUMS signature does not verify: not signed by $repo's release workflow for $version"
	fi
	say "signature checked"
elif [ "${CAIRN_REQUIRE_SIGNATURE:-}" = 1 ]; then
	die "CAIRN_REQUIRE_SIGNATURE=1 but cosign is not installed (https://docs.sigstore.dev/cosign/system_config/installation/)"
else
	say "note: signature not checked (install cosign to check it); the download is checked against SHA256SUMS"
fi

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
