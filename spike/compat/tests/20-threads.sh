#!/bin/bash
# Fifty threads with locks
set -euo pipefail
cd /tmp
python3 -c "
import threading
n = [0]; l = threading.Lock()
def f():
    for _ in range(10000):
        with l: n[0] += 1
ts = [threading.Thread(target=f) for _ in range(50)]
[t.start() for t in ts]; [t.join() for t in ts]
assert n[0] == 500000; print(\"ok\")"
