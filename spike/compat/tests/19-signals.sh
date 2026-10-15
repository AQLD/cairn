#!/bin/bash
# Background jobs, signals and traps
set -euo pipefail
cd /tmp
sleep 30 & p=$! && kill -TERM $p && ! wait $p; bash -c "trap \"echo trapped\" USR1; kill -USR1 \$\$; true" | grep -q trapped
