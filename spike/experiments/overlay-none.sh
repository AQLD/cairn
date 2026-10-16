#!/usr/bin/env bash
# Experiment: can a full save made with --overlay2=none (writes go straight to
# the host folder) be restored after the folder is tarred and re-extracted at a
# DIFFERENT path, as a copy or a move to another server would do?
set -euo pipefail
W=/var/lib/cairn-exp; rm -rf $W; mkdir -p $W
R="runsc --root $W/state --overlay2=none --network=none"

# a root filesystem with python
c=$(docker create python:3.12-slim); mkdir -p $W/ws1/rootfs; docker export $c | tar -x -C $W/ws1/rootfs; docker rm -f $c >/dev/null
cat > $W/ws1/rootfs/worker.py <<'PY'
import os, time, json
f = open("/data.log", "a", buffering=1)
n = 0
start = time.time()
while True:
    n += 1
    f.write(f"tick {n}\n")
    json.dump({"pid": os.getpid(), "start": start, "n": n, "off": f.tell()}, open("/state.json.tmp", "w")); os.replace("/state.json.tmp", "/state.json")
    time.sleep(0.1)
PY
mkdir -p $W/b1; (cd $W/b1 && runsc spec -- python3 -u /worker.py)
jq --arg r $W/ws1/rootfs '.root.path=$r | .root.readonly=false | .process.terminal=false' $W/b1/config.json > $W/b1/c && mv $W/b1/c $W/b1/config.json
$R run --detach --bundle $W/b1 ws1 </dev/null >/dev/null 2>&1
sleep 2; before=$($R exec ws1 cat /state.json); echo "before: $before"

$R checkpoint --image-path $W/img ws1 >/dev/null 2>&1
$R delete --force ws1 >/dev/null 2>&1 || true
echo "checkpoint files: $(ls $W/img | tr '\n' ' ') ($(du -sh $W/img | cut -f1))"

# tar the (now quiet) folder and extract it somewhere else, without preserving anything special
tar -C $W/ws1/rootfs -cf $W/rootfs.tar .
mkdir -p $W/ws2/rootfs; tar -C $W/ws2/rootfs -xf $W/rootfs.tar
mkdir -p $W/b2; jq --arg r $W/ws2/rootfs '.root.path=$r' $W/b1/config.json > $W/b2/config.json

restore() { $R restore --detach --image-path $W/img --bundle $1 $2 </dev/null >$W/restore-$2.log 2>&1; }
if restore $W/b2 ws2; then
  sleep 1; after=$($R exec ws2 cat /state.json); echo "after (other path): $after"
  python3 -c "import json,sys; b=json.loads('$before'); a=json.loads('$after'); ok = a['pid']==b['pid'] and a['start']==b['start'] and a['n']>b['n']; print('RESULT other-path:', 'PASS' if ok else 'FAIL')"
  tail -2 $W/ws2/rootfs/data.log
  $R kill ws2 KILL >/dev/null 2>&1 || true; $R delete --force ws2 >/dev/null 2>&1 || true
else
  echo "RESULT other-path: RESTORE FAILED"; tail -5 $W/restore-ws2.log
fi
