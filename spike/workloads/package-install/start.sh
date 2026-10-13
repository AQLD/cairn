#!/bin/sh
date +%s > /spike/started
/spike/install.sh &
echo $! > /spike/installer_pid
wait
# keep the sandbox alive after the install finishes so it can be probed
exec sleep infinity
