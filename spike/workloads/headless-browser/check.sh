#!/usr/bin/env bash
# Same browser process, same page (same page start time), counter still running.
set -euo pipefail
b=$1 a=$2
jq -e -n --slurpfile b "$b" --slurpfile a "$a" '
  ($b[0]) as $b | ($a[0]) as $a |
  if   $a.browser_pid != $b.browser_pid   then error("browser restarted")
  elif $a.page_started != $b.page_started then error("page reloaded (start time changed)")
  elif $a.count <= $b.count               then error("page script stopped (count \($b.count) -> \($a.count))")
  else "ok: same browser, page counter \($b.count) -> \($a.count)" end
' -r
