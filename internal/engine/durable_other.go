//go:build !linux

package engine

import (
	"os"
	"syscall"
)

// Only development runs on other systems (hosts run Linux), so durability here
// is best effort and cheap.

// syncTree does nothing here: without syncfs it would mean a global sync, or
// an fsync per file, on every workspace created.
func syncTree(dir string) error { return nil }

// fsync is a plain fsync, not os.File.Sync, which on macOS issues F_FULLFSYNC
// (a full drive cache flush, very slow).
func fsync(f *os.File) error { return syscall.Fsync(int(f.Fd())) }
