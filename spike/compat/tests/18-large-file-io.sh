#!/bin/bash
# Write, fsync and read back 256 MB
set -euo pipefail
cd /tmp
head -c 256M /dev/urandom > big && sync && a=$(sha256sum big | cut -d" " -f1) && cp big big2 && [ "$(sha256sum big2 | cut -d" " -f1)" = "$a" ]
