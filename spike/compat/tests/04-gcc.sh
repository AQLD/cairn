#!/bin/bash
# Compile and run a C program
set -euo pipefail
cd /tmp
printf "#include <stdio.h>\nint main(){puts(\"hi\");return 0;}\n" > h.c && gcc -O2 -o h h.c && ./h | grep -qx hi
