#!/bin/bash
# tar, gzip, xz and zip round trips
set -euo pipefail
cd /tmp
mkdir a && head -c 8M /dev/urandom > a/r && tar czf a.tgz a && tar cJf a.txz a && zip -qr a.zip a && rm -rf a && tar xzf a.tgz && rm -rf a && unzip -q a.zip && [ -s a/r ]
