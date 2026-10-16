package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/AQLD/cairn/internal/disk"
	"github.com/AQLD/cairn/internal/store"
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

// renameTo moves the filesystem to dst's name, unmounted. It is the commit
// point of building a filesystem, so it is durable: the contents are on disk
// before the rename, and the rename is on disk before it returns.
func (v volume) renameTo(dst volume) error {
	if !v.image {
		if err := syncTree(v.path); err != nil {
			return err
		}
		if err := os.Rename(v.path, dst.path); err != nil {
			return err
		}
		return syncParents(dst.path)
	}
	// never rename a mounted image: mounting it again under the new name would
	// put one filesystem on two devices
	if err := v.release(); err != nil { // unmounting writes the filesystem into the image
		return err
	}
	if err := syncFile(v.img()); err != nil { // and this, the image to disk
		return err
	}
	if err := os.Rename(v.img(), dst.img()); err != nil {
		return err
	}
	_ = os.Remove(v.path) // the empty mount point
	return syncParents(dst.img())
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

// checkDisk makes sure a workspace's disk is healthy before it is used. When
// the volume holding the sparse disk images fills up, a workspace writing at
// that moment gets I/O errors and its filesystem aborts its journal (it goes
// read-only, though the mount still says rw), and stays so after space is
// freed. Once there is room again, checkDisk repairs
// it: the sandbox is stopped, the filesystem checked and mounted again, and
// the workspace restarted, like a machine after a crash (files are kept,
// processes are not). Until there is room, it says the volume is full.
func (e *Engine) checkDisk(ctx context.Context, id string) error {
	w, err := e.liveWorkspace(ctx, id)
	if err != nil || w.State != store.Running {
		return nil // the operation itself reports the state
	}
	ws, err := specOf(w)
	if err != nil || !ws.DiskImage {
		return err
	}
	root := e.volume(id, "rootfs", ws)
	if ro, err := disk.Damaged(root.path); err != nil || !ro {
		return err
	}
	if err := e.needSpace("repairing the workspace's disk", 0); err != nil {
		return fmt.Errorf("%w (the volume holding workspaces filled up and this workspace's disk hit write errors; it is repaired on first use once space is freed)", err)
	}
	l := e.lock(id)
	if err := l.lockAlone(ctx, e.cfg.ExclusiveWait, "repairing its disk"); err != nil {
		return err
	}
	defer l.Unlock()
	if ro, err := disk.Damaged(root.path); err != nil || !ro {
		return err // repaired meanwhile
	}
	e.log.Warn("repairing a workspace disk that hit write errors (its volume filled up); processes restart", "workspace", id)
	spec, err := e.runtimeSpec(w)
	if err != nil {
		return err
	}
	if err := e.cfg.Runtime.Stop(ctx, spec); err != nil {
		return err
	}
	if err := root.release(); err != nil {
		return err
	}
	if err := disk.Repair(root.img()); err != nil {
		return err
	}
	if err := root.attach(); err != nil {
		return err
	}
	return e.cfg.Runtime.Start(ctx, spec)
}

// VolumeFree is the free space on the volume holding workspaces and saves.
func (e *Engine) VolumeFree() (uint64, error) { return freeBytes(e.cfg.DataDir) }
