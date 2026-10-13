#!/bin/bash
# HTTPS request with certificate checks
set -euo pipefail
cd /tmp
curl -sSf --max-time 20 https://example.com -o /dev/null
