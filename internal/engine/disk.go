package engine

import (
	"io"
	"io/fs"
	"path/filepath"
	"syscall"
)

// freeBytes is the space available to the engine on the filesystem holding path.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:unconvert // field types differ by OS
}

// dirSize is the total size of the regular files under root.
func dirSize(root string) (int64, error) {
	var n int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			n += info.Size()
		}
		return nil
	})
	return n, err
}

// failingReader stands in for a disk that fills up: it passes the first read
// through, then fails with err.
type failingReader struct {
	r    io.Reader
	err  error
	used bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.used {
		return 0, f.err
	}
	f.used = true
	return f.r.Read(p)
}
