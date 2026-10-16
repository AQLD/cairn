//go:build !linux

// Command cairn-init is process 1 inside gVisor workspaces; it only runs on Linux.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cairn-init runs inside Linux workspaces only")
	os.Exit(1)
}
