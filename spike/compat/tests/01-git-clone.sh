#!/bin/bash
# Clone a repository over HTTPS and read its history
set -euo pipefail
cd /tmp
git clone -q --depth 1 https://github.com/AQLD/cairn c && git -C c log --oneline -1
