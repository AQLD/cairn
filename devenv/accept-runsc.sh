#!/bin/bash
# Run the acceptance suite on gVisor (runsc), inside a Linux VM, as root.
# Usage: accept-runsc.sh REPO_DIR [go test args...]
set -e
mkdir -p /var/lib/cairn-test
if [ ! -s /var/lib/cairn-test/base.tar ]; then
  c=$(docker create python:3.12-slim); docker export $c > /var/lib/cairn-test/base.tar; docker rm -f $c >/dev/null
fi
export PATH=$PATH:/usr/local/go/bin GOFLAGS=-buildvcs=false GOCACHE=/var/lib/cairn-test/gocache GOMODCACHE=/var/lib/cairn-test/gomod
export CAIRN_TEST_RUNTIME=runsc CAIRN_TEST_TEMPLATE=/var/lib/cairn-test/base.tar
cd "$1"; shift
go test -count=1 -short ./acceptance "$@"
