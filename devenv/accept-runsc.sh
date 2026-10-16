#!/bin/bash
# Run the acceptance suite on gVisor (runsc), on a Linux host, as root (a dev
# VM or CI). Builds the templates the suite uses with Docker, once:
#   base.tar              python:3.12-slim, every workspace's default template
#   golden/<name>.tar     the E0 spike's workloads (G1.7); rebuilt when
#                         spike/workloads/<name> changes. CAIRN_GOLDEN=0 skips them.
# Usage: accept-runsc.sh REPO_DIR [go test args...]
set -euo pipefail
repo=$1; shift
cache=/var/lib/cairn-test
mkdir -p "$cache/golden"

export_image() { # IMAGE OUT.tar
  local c
  c=$(docker create "$1")
  docker export "$c" > "$2.tmp" && mv "$2.tmp" "$2"
  docker rm -f "$c" >/dev/null
}

[[ -s $cache/base.tar ]] || export_image python:3.12-slim "$cache/base.tar"

if [[ ${CAIRN_GOLDEN:-1} != 0 ]]; then
  for dir in "$repo"/spike/workloads/{python-open-files,node-dev-server,package-install,headless-browser}; do
    name=$(basename "$dir")
    sum=$(cd "$dir" && find . -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum | cut -c1-16)
    if [[ $(cat "$cache/golden/$name.sum" 2>/dev/null) != "$sum" || ! -s $cache/golden/$name.tar ]]; then
      echo "[accept-runsc] building golden workload $name" >&2
      docker build -q -t "cairn-golden/$name" "$dir" >/dev/null
      export_image "cairn-golden/$name" "$cache/golden/$name.tar"
      echo "$sum" > "$cache/golden/$name.sum"
    fi
  done
  export CAIRN_TEST_GOLDEN=$cache/golden
fi

export PATH=$PATH:/usr/local/go/bin GOFLAGS=-buildvcs=false GOCACHE=$cache/gocache GOMODCACHE=$cache/gomod
export CAIRN_TEST_RUNTIME=runsc CAIRN_TEST_TEMPLATE=$cache/base.tar
cd "$repo"
go test -count=1 -short ./acceptance "$@"
