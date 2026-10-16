#!/bin/bash
# What happens when a workspace exceeds its memory limit under gVisor: does the
# process die, or the whole sandbox? And what does the guest see?
set -u
export HOSTLIM=${HOSTLIM:-268435456} DATALIM=${DATALIM:-}
echo "host limit $HOSTLIM, RLIMIT_DATA ${DATALIM:-none}"
W=$(mktemp -d /var/tmp/memlimit.XXXX)
R="runsc --root $W/state --overlay2=none --network=none"
mkdir -p $W/b/rootfs
c=$(docker create python:3.12-slim); docker export $c | tar -x -C $W/b/rootfs; docker rm -f $c >/dev/null
(cd $W/b && runsc spec -- /bin/sh -c "trap 'exit 0' TERM; while :; do sleep 3600 & wait \$!; done")
jq --arg cg "/cairn-exp-$$" '.root.readonly=false | .linux.cgroupsPath=$cg | .linux.resources.memory={"limit": ($ENV.HOSTLIM|tonumber)} | .process.rlimits += (if $ENV.DATALIM != "" then [{"type":"RLIMIT_DATA","hard":($ENV.DATALIM|tonumber),"soft":($ENV.DATALIM|tonumber)}] else [] end)' $W/b/config.json > $W/b/c.json && mv $W/b/c.json $W/b/config.json
$R run --detach --bundle $W/b ws </dev/null >/dev/null 2>&1 || { echo "run failed"; exit 1; }
echo "guest MemTotal: $($R exec ws grep MemTotal /proc/meminfo)"
echo "guest ulimit -d: $($R exec ws sh -c 'ulimit -d')"
echo "host cgroup: $(cat /sys/fs/cgroup/cairn-exp-$$/memory.max 2>/dev/null || echo none)"
$R exec ws python3 -c 'b = bytearray(700 * 1024 * 1024); print("allocated 700MB")'; echo "alloc exit=$?"
echo "sandbox: $($R state ws | jq -r .status)"
$R exec ws python3 -c '
x = []
try:
    while True: x.append(bytearray(32 << 20))
except MemoryError: print("MemoryError after", len(x) * 32, "MB")'; echo "loop exit=$?"
echo "sandbox: $($R state ws 2>/dev/null | jq -r .status)"
dmesg 2>/dev/null | grep -i "oom\|killed process" | tail -3
$R delete --force ws >/dev/null 2>&1; umount $W/state/null-netns 2>/dev/null; rm -rf $W
