#!/bin/sh
# Starts cairnd in a container. Extra arguments are passed to cairnd.
#
# Under cgroup v2 a container starts in the root of its own cgroup namespace,
# and a cgroup that holds processes cannot hand controllers to children ("no
# internal processes"), so runsc fails to create a workspace's cgroup with
# "cgroup.subtree_control: device or resource busy". Kubernetes hides this;
# plain Docker (Docker Desktop, OrbStack, Linux) does not. Move every process
# here into a leaf cgroup and enable the controllers for the children, as
# docker-in-docker does. Without write access (not privileged) this is skipped
# and cairnd reports what it cannot do.
set -eu
cg=/sys/fs/cgroup
if [ -f "$cg/cgroup.controllers" ] && [ "$(cat /proc/self/cgroup)" = "0::/" ] && mkdir -p "$cg/engine" 2>/dev/null; then
	for pid in $(cat "$cg/cgroup.procs"); do
		echo "$pid" >"$cg/engine/cgroup.procs" 2>/dev/null || true
	done
	controllers=$(sed -e 's/^/+/' -e 's/ / +/g' "$cg/cgroup.controllers")
	if [ -n "$controllers" ] && ! echo "$controllers" >"$cg/cgroup.subtree_control" 2>/dev/null; then
		echo "cairn entrypoint: could not enable cgroup controllers ($controllers); is the container privileged?" >&2
	fi
fi
exec cairnd --data /var/lib/cairn --listen 0.0.0.0:7420 "$@"
