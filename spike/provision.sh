#!/usr/bin/env bash
# Create the two E0 test droplets on DigitalOcean: same region, different CPU types.
#
#   provision.sh SSH_KEY_ID_OR_FINGERPRINT [REGION]
#   destroy.sh                                  delete them (do this when done)
#
# Needs doctl, logged in (doctl auth init). Droplets bill hourly until destroyed.
set -euo pipefail

KEY=${1:?usage: provision.sh SSH_KEY_ID_OR_FINGERPRINT [REGION] (list keys: doctl compute ssh-key list)}
REGION=${2:-nyc3}
IMAGE=ubuntu-24-04-x64
TAG=cairn-spike

# Premium Intel vs Premium AMD: two different CPU families, which is what the
# portability half of the gate needs. Same size, so timings compare fairly.
NAMES=(cairn-spike-intel cairn-spike-amd)
SIZES=(s-2vcpu-4gb-intel s-2vcpu-4gb-amd)

command -v doctl >/dev/null || { echo "install doctl first: brew install doctl && doctl auth init" >&2; exit 1; }

echo "About to create in $REGION (tag $TAG):"
total=0
for i in 0 1; do
  name=${NAMES[$i]} size=${SIZES[$i]}
  hourly=$(doctl compute size list --output json | jq -r --arg s "$size" '.[] | select(.slug==$s) | .price_hourly')
  [[ -n $hourly ]] || { echo "size $size not available" >&2; exit 1; }
  printf "  %-20s %-20s \$%s/hour\n" "$name" "$size" "$hourly"
  total=$(python3 -c "print(round($total + $hourly, 4))")
done
echo "  total about \$$total/hour, billed until you run spike/destroy.sh"
read -r -p "Create them? [y/N] " ok
[[ $ok == y || $ok == Y ]] || { echo "cancelled"; exit 1; }

for i in 0 1; do
  doctl compute droplet create "${NAMES[$i]}" --region "$REGION" --size "${SIZES[$i]}" --image "$IMAGE" \
    --ssh-keys "$KEY" --tag-name "$TAG" --enable-private-networking --wait --format Name,PublicIPv4,PrivateIPv4 --no-header
done
echo
echo "Next: spike/cross.sh sync <intel-ip> <amd-ip> && spike/cross.sh all <intel-ip> <amd-ip>"
