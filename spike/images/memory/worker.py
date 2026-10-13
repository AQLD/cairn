"""Hold MEM_MB of random bytes in memory and prove they survive a restore.

Random data is the worst case for save size and time (nothing to compress or
skip). The worker hashes the buffer once after filling it; on request it hashes
it again, so a check after restore proves every byte came back.
"""
import hashlib
import json
import os
import time

MB = int(os.environ["MEM_MB"])
START = time.time()

buf = bytearray(MB * 1024 * 1024)
CHUNK = 16 << 20
for off in range(0, len(buf), CHUNK):
    end = min(off + CHUNK, len(buf))
    buf[off:end] = os.urandom(end - off)
baseline = hashlib.sha256(buf).hexdigest()
filled = time.time()

counter = 0
verified_sha = None
verified_at = 0.0
while True:
    counter += 1
    if os.path.exists("/spike/verify-request"):
        os.remove("/spike/verify-request")
        verified_sha = hashlib.sha256(buf).hexdigest()
        verified_at = time.time()
    state = {
        "ready": True,
        "pid": os.getpid(),
        "started": START,
        "fill_seconds": round(filled - START, 2),
        "mem_mb": MB,
        "counter": counter,
        "baseline_sha": baseline,
        "verified_sha": verified_sha,
        "verified_at": verified_at,
    }
    with open("/spike/state.json.tmp", "w") as f:
        json.dump(state, f)
    os.replace("/spike/state.json.tmp", "/spike/state.json")
    time.sleep(0.2)
