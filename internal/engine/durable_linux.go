//go:build linux

package engine

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncTree writes everything under dir to disk: syncfs on its filesystem,
// one call instead of one fsync per extracted file.
func syncTree(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}

// fsync writes a file's (or folder's) data and metadata to disk.
func fsync(f *os.File) error { return f.Sync() }
