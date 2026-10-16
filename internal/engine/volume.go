package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/AQLD/cairn/internal/disk"
)

// DefaultDiskMB is the disk size of a workspace created without one.
const DefaultDiskMB = 10 << 10

// A volume is one of a workspace's filesystems: the live one ("rootfs") or one
// being built next to it ("rootfs.new" from a template, "rootfs.next" from a
// save, "rootfs.prev" on its way out).
//
// Workspaces with a disk limit keep each filesystem in an image file
// (path+".img") mounted at path; the rest use a plain folder at path. Either
// way, a filesystem is built under its own name and renamed into place, and the
// rename is the commit point recovery relies on: for an image that is the
// image file's rename, since the folder is only where it is mounted.
type volume struct {
	path   string
	image  bool
	sizeMB int64
}

func (e *Engine) volume(id, name string, spec Spec) volume {
	return volume{path: filepath.Join(e.workspaceDir(id), name), image: spec.DiskImage, sizeMB: spec.Limits.DiskMB}
}

func (v volume) img() string { return v.path + ".img" }

// exists reports whether the filesystem has been created.
func (v volume) exists() bool {
	p := v.path
	if v.image {
		p = v.img()
	}
	_, err := os.Stat(p)
	return err == nil
}

// prepare creates the empty filesystem and makes it writable at path.
func (v volume) prepare() error {
	if !v.image {
		return os.MkdirAll(v.path, 0o755)
	}
	if err := os.MkdirAll(v.path, 0o755); err != nil {
		return err
	}
	if err := disk.Create(v.img(), v.sizeMB); err != nil {
		return err
	}
	if err := disk.Mount(v.img(), v.path); err != nil {
		_ = os.Remove(v.img())
		return err
	}
	// mkfs's lost+found would show up in the workspace; fsck recreates it if needed
	_ = os.Remove(filepath.Join(v.path, "lost+found"))
	return nil
}

// release unmounts an image so it can be renamed or mounted elsewhere.
func (v volume) release() error {
	if !v.image {
		return nil
	}
	return disk.Unmount(v.path)
}

// attach mounts an existing image at path, if it is not already.
func (v volume) attach() error {
	if !v.image || !v.exists() {
		return nil
	}
	if mounted, err := disk.Mounted(v.path); err != nil || mounted {
		return err
	}
	if err := os.MkdirAll(v.path, 0o755); err != nil {
		return err
	}
	return disk.Mount(v.img(), v.path)
}

// renameTo moves the filesystem to dst's name, unmounted.
func (v volume) renameTo(dst volume) error {
	if !v.image {
		return os.Rename(v.path, dst.path)
	}
	// never rename a mounted image: mounting it again under the new name would
	// put one filesystem on two devices
	if err := v.release(); err != nil {
		return err
	}
	if err := os.Rename(v.img(), dst.img()); err != nil {
		return err
	}
	_ = os.Remove(v.path) // the empty mount point
	return nil
}

// remove deletes the filesystem.
func (v volume) remove() error {
	if !v.image {
		return os.RemoveAll(v.path)
	}
	// unmount first: removing the folder would otherwise empty the image's
	// contents one file at a time, and fail on the mount point anyway
	if err := disk.Unmount(v.path); err != nil {
		return err
	}
	if err := os.RemoveAll(v.path); err != nil {
		return err
	}
	if err := os.Remove(v.img()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// removeWorkspaceDir deletes a workspace's folder with every filesystem in it.
func (e *Engine) removeWorkspaceDir(id string, spec Spec) error {
	for _, name := range []string{"rootfs", "rootfs.new", "rootfs.next", "rootfs.prev"} {
		if err := e.volume(id, name, spec).remove(); err != nil {
			return err
		}
	}
	return os.RemoveAll(e.workspaceDir(id))
}

// diskFor fills in how a new workspace stores its files: an image of the
// requested (or default) size where images work, a plain folder otherwise.
func (e *Engine) diskFor(spec *Spec) {
	if e.diskErr != nil {
		spec.DiskImage = false
		return
	}
	if spec.Limits.DiskMB == 0 {
		spec.Limits.DiskMB = e.cfg.DiskMB
	}
	spec.DiskImage = true
}

// diskFull turns "no space left" from a workspace's own disk into an error
// that says so; other errors pass through.
func diskFull(err error, spec Spec) error {
	if err == nil || !spec.DiskImage || !(errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)) {
		return err
	}
	return fmt.Errorf("%w: the workspace's disk is full (limit %d MB)", ErrInsufficientStorage, spec.Limits.DiskMB)
}
