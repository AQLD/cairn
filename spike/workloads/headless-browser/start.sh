#!/bin/sh
chromium --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage \
  --user-data-dir=/tmp/chrome --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 \
  file:///spike/page.html > /spike/chromium.log 2>&1 &
echo $! > /spike/browser_pid
wait
