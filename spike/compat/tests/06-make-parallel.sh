#!/bin/bash
# Parallel make of 40 C files
set -euo pipefail
cd /tmp
mkdir m && cd m && for i in $(seq 1 40); do printf "int f%d(void){return %d;}\n" $i $i > f$i.c; done && printf "int main(void){return 0;}\n" > main.c && printf "SRCS:=\$(wildcard *.c)\nOBJS:=\$(SRCS:.c=.o)\napp: \$(OBJS)\n\t\$(CC) -o \$@ \$^\n" > Makefile && make -s -j4 && ./app
