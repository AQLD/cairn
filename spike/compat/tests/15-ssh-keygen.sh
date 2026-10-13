#!/bin/bash
# Generate an SSH key
set -euo pipefail
cd /tmp
ssh-keygen -q -t ed25519 -N "" -f k && [ -s k.pub ]
