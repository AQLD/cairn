package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
//
// A filesystem built on a base (a template's files) can instead be layered:
// an overlay mounted at path of the base's shared, read-only root
// (bases/<sha>.root) and the filesystem's own changes. The changes live in a
// layer, path+".layer" (a folder, or the image path+".layer.img" mounted
// there), holding upper/ and work/ for overlayfs and base, the base's sha.
// The layer is what is built and renamed; path is only where the overlay is
// mounted. Programs, saves and the file API see an ordinary folder at path.
type volume struct {
	path     string
	image    bool
	sizeMB   int64
	basesDir string // where base roots live (bases/<sha>.root)
	// lower, when set, makes prepare build a layered filesystem on the base
	// with this sha (its root must exist). Existing filesystems say for
	// themselves whether they are layered.
	lower string
}

func (e *Engine) volume(id, name string, spec Spec) volume {
	return volume{path: filepath.Join(e.workspaceDir(id), name), image: spec.DiskImage, sizeMB: spec.Limits.DiskMB, basesDir: e.basesDir()}
}

func (v volume) img() string { return v.path + ".img" }

// diskDir and diskImg are where the filesystem's own image is mounted and
// stored: the layer's, for a layered filesystem.
func (v volume) diskDir() string {
	if v.layered() {
		return v.layer()
	}
	return v.path
}

func (v volume) diskImg() string {
	if v.layered() {
		return v.layerImg()
	}
	return v.img()
}

func (v volume) layer() string    { return v.path + ".layer" }
func (v volume) layerImg() string { return v.layer() + ".img" }

// layered reports whether the filesystem is an overlay on a base.
func (v volume) layered() bool {
	p := v.layer()
	if v.image {
		p = v.layerImg()
	}
	_, err := os.Stat(p)
	return err == nil
}

// exists reports whether the filesystem has been created.
func (v volume) exists() bool {
	p := v.path
	if v.image {
		p = v.img()
	}
	_, err := os.Stat(p)
	return err == nil || v.layered()
}

// prepare creates the empty filesystem (layered on v.lower if set) and makes
// it writable at path.
func (v volume) prepare() error {
	if v.lower != "" {
		return v.prepareLayered()
	}
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
	if v.layered() {
		if err := disk.Unmount(v.path); err != nil {
			return err
		}
		if v.image {
			return disk.Unmount(v.layer())
		}
		return nil
	}
	if !v.image {
		return nil
	}
	return disk.Unmount(v.path)
}

// attach mounts an existing image at path, if it is not already.
func (v volume) attach() error {
	if v.layered() {
		return v.attachLayered()
	}
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
	if v.layered() {
		return v.renameLayered(dst)
	}
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
	if err := v.removeLayered(); err != nil {
		return err
	}
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
	if e.pressured.Load() {
		// the disk pressure guard has frozen workspace filesystems: a command
		// or a write would wait for room, so callers hear why at once
		return errPressure
	}
	root := e.volume(id, "rootfs", ws)
	_, flagged := e.needsRepair.Load(id)
	if ro, err := disk.Damaged(root.diskDir()); err != nil || (!ro && !flagged) {
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
	_, flagged = e.needsRepair.Load(id)
	if ro, err := disk.Damaged(root.diskDir()); err != nil || (!ro && !flagged) {
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
	if err := disk.Repair(root.diskImg()); err != nil {
		return err
	}
	e.needsRepair.Delete(id)
	if err := root.attach(); err != nil {
		return err
	}
	return e.cfg.Runtime.Start(ctx, spec)
}

// VolumeFree is the free space on the volume holding workspaces and saves.
func (e *Engine) VolumeFree() (uint64, error) { return freeBytes(e.cfg.DataDir) }

// --- layered filesystems ---

// rootLayered reports whether a workspace's live filesystem is layered.
func (e *Engine) rootLayered(id string) bool {
	_, err := os.Stat(filepath.Join(e.workspaceDir(id), "rootfs.layer"))
	return err == nil
}

func (v volume) baseRoot(sha string) string { return filepath.Join(v.basesDir, sha+".root") }

// prepareLayered creates an empty layer on base v.lower and mounts the
// overlay at path.
func (v volume) prepareLayered() error {
	if err := os.MkdirAll(v.layer(), 0o755); err != nil {
		return err
	}
	if v.image {
		if err := disk.Create(v.layerImg(), v.sizeMB); err != nil {
			return err
		}
		if err := disk.Mount(v.layerImg(), v.layer()); err != nil {
			_ = os.Remove(v.layerImg())
			return err
		}
		_ = os.Remove(filepath.Join(v.layer(), "lost+found"))
	}
	for _, d := range []string{"upper", "work"} {
		if err := os.Mkdir(filepath.Join(v.layer(), d), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(v.layer(), "base"), []byte(v.lower+"\n"), 0o600); err != nil {
		return err
	}
	return v.mountOverlay(v.lower)
}

// attachLayered mounts an existing layer (if an image) and the overlay, if
// they are not already: after an engine restart they usually are, after a
// host restart they are not.
func (v volume) attachLayered() error {
	if v.image {
		if mounted, err := disk.Mounted(v.layer()); err != nil {
			return err
		} else if !mounted {
			if err := os.MkdirAll(v.layer(), 0o755); err != nil {
				return err
			}
			if err := disk.Mount(v.layerImg(), v.layer()); err != nil {
				return err
			}
		}
	}
	if mounted, err := disk.Mounted(v.path); err != nil || mounted {
		return err
	}
	data, err := os.ReadFile(filepath.Join(v.layer(), "base"))
	if err != nil {
		return fmt.Errorf("layered filesystem %s names no base: %w", v.path, err)
	}
	return v.mountOverlay(strings.TrimSpace(string(data)))
}

func (v volume) mountOverlay(sha string) error {
	if len(sha) != 64 || strings.Trim(sha, "0123456789abcdef") != "" {
		return fmt.Errorf("layered filesystem %s: invalid base %q", v.path, sha)
	}
	if _, err := os.Stat(v.baseRoot(sha)); err != nil {
		return fmt.Errorf("layered filesystem %s: base root: %w", v.path, err)
	}
	if err := os.MkdirAll(v.path, 0o755); err != nil {
		return err
	}
	return disk.MountOverlay(v.baseRoot(sha), filepath.Join(v.layer(), "upper"), filepath.Join(v.layer(), "work"), v.path)
}

// renameLayered moves the layer to dst's name, unmounted and durably (see
// renameTo).
func (v volume) renameLayered(dst volume) error {
	if err := v.release(); err != nil {
		return err
	}
	if v.image {
		if err := syncFile(v.layerImg()); err != nil {
			return err
		}
		if err := os.Rename(v.layerImg(), dst.layerImg()); err != nil {
			return err
		}
		_ = os.Remove(v.layer())
		_ = os.Remove(v.path)
		return syncParents(dst.layerImg())
	}
	if err := syncTree(v.layer()); err != nil {
		return err
	}
	if err := os.Rename(v.layer(), dst.layer()); err != nil {
		return err
	}
	_ = os.Remove(v.path)
	return syncParents(dst.layer())
}

// removeLayered deletes a layered filesystem, if there is one: overlay first
// (removing through it would write whiteouts into the layer, file by file),
// then the layer.
func (v volume) removeLayered() error {
	if !v.layered() {
		if _, err := os.Stat(v.layer()); err != nil {
			return nil // nothing layered here (a leftover empty mount point is removed below)
		}
	}
	if err := disk.Unmount(v.path); err != nil {
		return err
	}
	if err := disk.Unmount(v.layer()); err != nil {
		return err
	}
	if err := os.RemoveAll(v.layer()); err != nil {
		return err
	}
	if err := os.Remove(v.layerImg()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
