# A shell session: one shell that sources each cell, so the working folder,
# variables and functions carry over between calls. Same protocol as
# python.py. `exit` in a cell ends the session; the driver's own variables
# start with __cairn_ so cells do not clobber them.
__cairn_d=$1 __cairn_sh=$2
exec 4<>"$__cairn_d/done" # held open: writing never blocks
trap 'printf "\ncairn: interrupted\n" >&2' INT
while :; do
  read -r __cairn_n < "$__cairn_d/go" || continue
  [ -n "$__cairn_n" ] || continue
  echo "$__cairn_n" > "$__cairn_d/running"
  if "$__cairn_sh" -n "$__cairn_d/cell" 2> "$__cairn_d/err.$__cairn_n"; then # a syntax error would end a sourcing shell
    . "$__cairn_d/cell" > "$__cairn_d/out.$__cairn_n" 2> "$__cairn_d/err.$__cairn_n" < /dev/null 4>&-
    __cairn_status=$?
  else
    : > "$__cairn_d/out.$__cairn_n"
    __cairn_status=2
  fi
  rm -f "$__cairn_d/running"
  echo "$__cairn_n $__cairn_status" >&4
done
