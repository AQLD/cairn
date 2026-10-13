#!/usr/bin/env bash
# Delete every droplet tagged cairn-spike. Copy results first: they live on your machine
# (spike/results/), so nothing on the droplets is needed afterwards.
set -euo pipefail
doctl compute droplet list --tag-name cairn-spike --format Name,PublicIPv4 --no-header
read -r -p "Delete these droplets? [y/N] " ok
[[ $ok == y || $ok == Y ]] || { echo "cancelled"; exit 1; }
doctl compute droplet delete --tag-name cairn-spike --force
echo "deleted"
