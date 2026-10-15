#!/bin/bash
# ripgrep over many files
set -euo pipefail
cd /tmp
mkdir r && for i in $(seq 1 500); do echo "line $i needle$((i % 7))" > r/f$i.txt; done && [ "$(rg -l needle3 r | wc -l)" -gt 60 ]
