#!/bin/bash
# ps, top, free and /proc
set -euo pipefail
cd /tmp
ps aux >/dev/null && top -bn1 | head -5 >/dev/null && free -m >/dev/null && grep -q VmRSS /proc/self/status && cat /proc/cpuinfo >/dev/null
