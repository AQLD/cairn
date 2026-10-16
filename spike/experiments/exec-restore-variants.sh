#!/bin/bash
# Which background processes survive checkpoint + restore under gVisor?
#   A  a child of `runsc exec`            B  the same in its own session (setsid)
#   C  `runsc exec --detach`              D  a child of the container's init
# Result (release 20261005): only D survives; A-C are SIGKILLed on restore.
# This is why every workspace runs cairn-init as process 1 (cmd/cairn-init).
set -u
W=$(mktemp -d /var/tmp/exec-variants.XXXX)
R="runsc --root $W/state --overlay2=none --network=none"
mkdir -p $W/b/rootfs
c=$(docker create python:3.12-slim); docker export $c | tar -x -C $W/b/rootfs; docker rm -f $c >/dev/null
printf 'import time,sys\nn=0\nwhile True:\n    n+=1; open("/root/c"+sys.argv[1],"w").write(str(n)); time.sleep(0.1)\n' > $W/b/rootfs/counter.py
# init: starts anything written to /run/spawn (one command per line), so those
# processes are children of the container's init rather than of an exec
mkdir -p $W/b/rootfs/run
(cd $W/b && runsc spec -- /bin/sh -c 'mkfifo /run/spawn; while :; do while read -r cmd; do sh -c "$cmd" </dev/null >/dev/null 2>&1 & done < /run/spawn; done')
jq '.root.readonly=false' $W/b/config.json > $W/b/c.json && mv $W/b/c.json $W/b/config.json
$R run --detach --bundle $W/b ws </dev/null >/dev/null 2>&1 || { echo "run failed"; exit 1; }
sleep 0.5
$R exec ws sh -c 'nohup python3 /counter.py A </dev/null >/dev/null 2>&1 &'          # A: exec child
$R exec ws sh -c 'setsid -f python3 /counter.py B </dev/null >/dev/null 2>&1'         # B: exec child in its own session
$R exec --detach ws python3 /counter.py C </dev/null >/dev/null 2>&1                  # C: runsc exec --detach
$R exec ws sh -c 'echo "python3 /counter.py D" > /run/spawn'                          # D: child of init
sleep 2
check() { for v in A B C D; do a=$($R exec ws cat /root/c$v 2>/dev/null); sleep 0.4; b=$($R exec ws cat /root/c$v 2>/dev/null); [[ -n $a && $a != "$b" ]] && s=running || s=DEAD; echo "  $v: $s ($a -> $b)"; done; }
echo "before:"; check
$R checkpoint --image-path $W/img ws >$W/cp.log 2>&1 || { echo "checkpoint failed"; tail -3 $W/cp.log; }
$R delete --force ws >/dev/null 2>&1
$R restore --detach --image-path $W/img --bundle $W/b ws </dev/null >$W/restore.log 2>&1 || { echo "restore failed"; tail -3 $W/restore.log; }
sleep 1; echo "after restore:"; check
$R delete --force ws >/dev/null 2>&1; umount $W/state/null-netns 2>/dev/null; rm -rf $W
