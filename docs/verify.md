# Verifying a release

Each release from v0.3.0 on ships, for every platform, an archive and a CycloneDX bill of materials (`*.cdx.json`). It also ships `SHA256SUMS` over all of them, and `SHA256SUMS.sigstore.json`, a Sigstore signature made by the release workflow itself. There's no long-lived key to leak. The signature names the workflow and tag that made it, and it is recorded in Sigstore's public transparency log.

`install.sh` checks all of this when [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) is installed. Set `CAIRN_REQUIRE_SIGNATURE=1` to refuse to install without checking. To check by hand:

```sh
v=v0.3.0
base=https://github.com/AQLD/cairn/releases/download/$v
curl -fsSLO $base/SHA256SUMS -O $base/SHA256SUMS.sigstore.json -O $base/cairn_${v}_linux_amd64.tar.gz

# 1. SHA256SUMS was made by AQLD/cairn's release workflow for this tag
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/AQLD/cairn/.github/workflows/release.yml@refs/tags/$v" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS

# 2. the archive is the one it lists
sha256sum --ignore-missing -c SHA256SUMS
```

The bill of materials lists every Go module linked into the binaries, at the exact version and checksum they were built with, and the Go version. It's read from the binaries themselves (`internal/tools/sbom`), not from `go.mod`, so it describes what ships. `deploy/release.sh` rebuilds a release anywhere Go runs:

```sh
deploy/release.sh v0.3.0 dist
```
