#!/usr/bin/env bash
# Builds a release into OUTDIR: for each platform an archive with the
# binaries and a CycloneDX bill of materials of exactly what they link, and
# SHA256SUMS over all of them. The release workflow signs SHA256SUMS; this
# script needs nothing but Go, so a release can be rebuilt and checked
# anywhere.
#
#   deploy/release.sh v0.3.0 dist
#   CAIRN_RELEASE_TARGETS="linux/arm64" deploy/release.sh v0.0.0-test /tmp/out
set -euo pipefail

ver=${1:?usage: release.sh VERSION OUTDIR}
out=${2:?usage: release.sh VERSION OUTDIR}
targets=${CAIRN_RELEASE_TARGETS:-"darwin/arm64 darwin/amd64 linux/arm64 linux/amd64"}
repo=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

sbomtool="$work/sbom"
(cd "$repo" && CGO_ENABLED=0 go build -o "$sbomtool" ./internal/tools/sbom)

for target in $targets; do
	os=${target%/*} arch=${target#*/}
	name=cairn_${ver}_${os}_${arch}
	dir=$work/$name
	mkdir -p "$dir"
	cmds=./cmd/cairn
	[[ $os == linux ]] && cmds="./cmd/cairn ./cmd/cairnd ./cmd/cairn-init"
	(cd "$repo" && GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=$ver" -o "$dir/" $cmds)
	cp "$repo/LICENSE" "$repo/README.md" "$dir/"
	bins=()
	for b in cairn cairnd cairn-init; do [[ -f $dir/$b ]] && bins+=("$dir/$b"); done
	"$sbomtool" -name cairn -version "$ver" "${bins[@]}" > "$out/$name.cdx.json"
	tar -C "$work" -czf "$out/$name.tar.gz" "$name"
done
(cd "$out" && { if command -v sha256sum >/dev/null; then sha256sum ./*.tar.gz ./*.cdx.json; else shasum -a 256 ./*.tar.gz ./*.cdx.json; fi; } | sed 's#  \./#  #' > SHA256SUMS)
echo "release $ver in $out" >&2
