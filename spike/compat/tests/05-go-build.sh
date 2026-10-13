#!/bin/bash
# Build and run a Go program
set -euo pipefail
cd /tmp
mkdir g && cd g && printf "package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"hi\")}\n" > main.go && GOCACHE=/tmp/gocache GOPATH=/tmp/gopath GO111MODULE=off go build -o h main.go && ./h | grep -qx hi
