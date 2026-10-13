#!/bin/bash
# Unix domain sockets and socketpair
set -euo pipefail
cd /tmp
python3 -c "
import socket, os, threading
a, b = socket.socketpair(); a.sendall(b\"x\"); assert b.recv(1) == b\"x\"
s = socket.socket(socket.AF_UNIX); s.bind(\"/tmp/s.sock\"); s.listen(1)
threading.Thread(target=lambda: socket.socket(socket.AF_UNIX).connect(\"/tmp/s.sock\")).start()
c, _ = s.accept(); print(\"ok\")"
