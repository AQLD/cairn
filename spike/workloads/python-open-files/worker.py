"""A long-running Python process that keeps files open and state in memory.

Every 100 ms it appends a numbered line to a log it holds open, and reads the
next line of a file it holds open for reading. If the process survives a
checkpoint and restore, the counter keeps rising from where it was, the PID and
start time are unchanged, and the open files' offsets still match their content.
"""
import json
import os
import time

START = time.time()
os.makedirs("/data", exist_ok=True)

# A file held open for appending: its offset must survive the restore.
log = open("/data/log.txt", "a", buffering=1)

# A file held open for reading, consumed one line per tick.
with open("/data/input.txt", "w") as f:
    for i in range(1_000_000):
        f.write(f"line {i}\n")
reader = open("/data/input.txt", "r")

counter = 0
gaps = 0
expected_line = 0
while True:
    counter += 1
    log.write(f"tick {counter}\n")
    line = reader.readline().strip()
    if line != f"line {expected_line}":
        gaps += 1
    expected_line += 1

    state = {
        "ready": counter >= 20,
        "pid": os.getpid(),
        "started": START,
        "counter": counter,
        "log_offset": log.tell(),
        "log_size": os.fstat(log.fileno()).st_size,
        "reader_line": expected_line,
        "read_gaps": gaps,
        "uptime": time.time() - START,
    }
    tmp = "/spike/state.json.tmp"
    with open(tmp, "w") as f:
        json.dump(state, f)
    os.replace(tmp, "/spike/state.json")
    time.sleep(0.1)
