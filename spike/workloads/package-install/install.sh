#!/bin/sh
# Download a large package (the CPU build of PyTorch, ~100-200 MB) the way an
# agent installing dependencies would. The checkpoint lands mid-download; after
# the restore the open HTTPS connection is gone, so pip must retry and finish.
mkdir -p /data/dl
attempt=0
while [ $attempt -lt 5 ]; do
  attempt=$((attempt + 1))
  echo "$attempt" > /spike/attempts
  if pip download --no-deps --retries 10 --timeout 20 --progress-bar off \
       --index-url https://download.pytorch.org/whl/cpu torch==2.4.1 -d /data/dl \
       > /spike/pip.log 2>&1; then
    echo 0 > /spike/exit_code
    exit 0
  fi
done
echo 1 > /spike/exit_code
