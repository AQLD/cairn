#!/bin/bash
# Process pool and shared memory
set -euo pipefail
cd /tmp
python3 -c "
from multiprocessing import Pool, shared_memory
with Pool(4) as p: assert sum(p.map(abs, range(-1000, 1000))) == 1000000
m = shared_memory.SharedMemory(create=True, size=1<<20); m.buf[0] = 7; assert m.buf[0] == 7; m.close(); m.unlink()
print(\"ok\")"
