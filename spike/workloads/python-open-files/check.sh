#!/usr/bin/env bash
# Same process kept running: same PID and start time, counter still rising,
# open-file offsets consistent with the files, no lines skipped by the reader.
set -euo pipefail
b=$1 a=$2
jq -e -n --slurpfile b "$b" --slurpfile a "$a" '
  ($b[0]) as $b | ($a[0]) as $a |
  if   $a.pid != $b.pid             then error("pid changed \($b.pid) -> \($a.pid): process restarted")
  elif $a.started != $b.started     then error("start time changed: process restarted")
  elif $a.counter <= $b.counter     then error("counter did not advance (\($b.counter) -> \($a.counter))")
  elif $a.log_offset != $a.log_size then error("append offset \($a.log_offset) != file size \($a.log_size)")
  elif $a.read_gaps != 0            then error("reader skipped or repeated \($a.read_gaps) lines")
  else "ok: counter \($b.counter) -> \($a.counter), pid \($a.pid) kept, offsets consistent" end
' -r
