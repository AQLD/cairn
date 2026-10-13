#!/bin/bash
# npm install a package with dependencies
set -euo pipefail
cd /tmp
mkdir n && cd n && npm init -y >/dev/null && npm install --silent --no-audit --no-fund express && node -e "require(\"express\"); console.log(\"ok\")"
