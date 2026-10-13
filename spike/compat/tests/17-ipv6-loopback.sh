#!/bin/bash
# IPv6 loopback listen and connect
set -euo pipefail
cd /tmp
python3 -c "
import socket
s = socket.socket(socket.AF_INET6); s.bind((\"::1\", 0)); s.listen(1)
c = socket.socket(socket.AF_INET6); c.connect(s.getsockname()[:2]); print(\"ok\")"
