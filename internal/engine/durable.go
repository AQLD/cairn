package engine

import (
	"os"
	"path/filepath"
)

// Power-cut safety. A new file or folder survives a power cut only once its
// data is on disk and so is the folder entry naming it: rename alone is
// atomic, not durable. Saves already do this (internal/save); these helpers
// do it for workspace filesystems and templates.

// syncDir writes a folder's entries (creations, renames) to disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return fsync(d)
}

// syncParents makes path's own entry durable, and its parent's (path may be
// in a folder created just now, such as a new workspace's).
func syncParents(path string) error {
	parent := filepath.Dir(path)
	if err := syncDir(parent); err != nil {
		return err
	}
	return syncDir(filepath.Dir(parent))
}

// syncFile writes a file's data and metadata to disk.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return fsync(f)
}
