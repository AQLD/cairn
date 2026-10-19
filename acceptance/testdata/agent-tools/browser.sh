#!/bin/sh
# Start the web app and a headless browser with its DevTools endpoint on
# 127.0.0.1:9222. Pids go to /agent/run so tests can check they are the same
# processes after a resume.
mkdir -p /agent/run /root/outbox
python3 /agent/site.py > /agent/run/site.log 2>&1 &
echo $! > /agent/run/site.pid
chromium --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage \
  --user-data-dir=/tmp/chrome --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 \
  --remote-allow-origins='*' about:blank > /agent/run/chromium.log 2>&1 &
echo $! > /agent/run/browser.pid
wait
