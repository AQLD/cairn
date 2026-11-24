package engine

// The disk pressure guard. Workspace disks are sparse images, so their sizes
// can add up to more than the volume holding them. When that volume fills, a
// write inside a workspace cannot reach the disk, and depending on timing the
// program is told so or is not (the kernel logs I/O errors and data loss
// while fsync returns success; deterministically so on an overlay on Linux
// 7.0). The guard never lets it get there: when the volume's free space drops
// below a reserve, every workspace filesystem is frozen, which writes out what
// it holds while there is room and then makes programs' writes wait; API
// writes are refused at once with insufficient_storage. When there is room
// again they are thawed and the waiting writes go through. Nothing is ever
// written to a full volume, so nothing written is lost. Reads keep working.

import (
	"context"
	"time"

	"github.com/AQLD/cairn/internal/disk"
	"github.com/AQLD/cairn/internal/store"
)

// DefaultDiskReserve is the free space below which workspace disks turn
// read-only. It must cover what workspaces can have written but not yet
// flushed when the guard trips, and what the volume fills by between checks.
const DefaultDiskReserve = 1 << 30

// pressureInterval is how often the guard checks the volume.
const pressureInterval = 250 * time.Millisecond

// pressureGuard runs until ctx ends.
func (e *Engine) pressureGuard(ctx context.Context) {
	defer close(e.pressureDone)
	tick := time.NewTicker(pressureInterval)
	defer tick.Stop()
	for {
		e.checkPressure(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// checkPressure trips or releases the guard. Released only with the reserve
// plus a margin free, so a volume hovering at the line does not flap.
func (e *Engine) checkPressure(ctx context.Context) {
	free, err := freeBytes(e.cfg.DataDir)
	if err != nil {
		return
	}
	reserve := uint64(e.cfg.DiskReserve)
	switch {
	case !e.pressured.Load() && free < reserve:
		e.pressured.Store(true)
		e.log.Error("the volume holding workspaces is nearly full: workspace writes wait until there is room",
			"free_mb", free>>20, "reserve_mb", reserve>>20)
		e.setDisksReadOnly(ctx, true)
	case e.pressured.Load() && free >= reserve+reserve/4:
		e.pressured.Store(false)
		e.log.Info("room on the volume again: workspace writes go through", "free_mb", free>>20)
		e.setDisksReadOnly(ctx, false)
	case e.pressured.Load():
		e.setDisksReadOnly(ctx, true) // disks mounted since (a wake) follow too
	}
}

// setDisksReadOnly freezes (ro) or thaws every live workspace's image
// filesystem.
func (e *Engine) setDisksReadOnly(ctx context.Context, ro bool) {
	ws, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return
	}
	for i := range ws {
		if ws[i].State == store.Deleted {
			continue
		}
		spec, err := specOf(&ws[i])
		if err != nil || !spec.DiskImage {
			continue
		}
		dir := e.volume(ws[i].ID, "rootfs", spec).diskDir()
		if mounted, err := disk.Mounted(dir); err != nil || !mounted {
			continue
		}
		if _, broken := e.needsRepair.Load(ws[i].ID); broken && ro {
			continue // its freeze failed: repaired once there is room
		}
		op := disk.Thaw
		if ro {
			op = disk.Freeze
		}
		if err := op(dir); err != nil {
			e.log.Warn("could not change a workspace disk's write access", "workspace", ws[i].ID, "read_only", ro, "err", err)
			if ro {
				// The volume filled before the guard could act (an outside
				// process filling it faster than the guard checks): the freeze
				// could not write out what the filesystem held. Leave it as it
				// was and repair it on first use once there is room (checkDisk),
				// as a disk that hit write errors.
				_ = disk.Thaw(dir)
				e.needsRepair.Store(ws[i].ID, true)
			}
		}
	}
}

// thawWorkspace thaws one workspace's image filesystem, if mounted (frozen
// or not: thawing a filesystem that is not frozen does nothing).
func (e *Engine) thawWorkspace(w *store.Workspace) {
	spec, err := specOf(w)
	if err != nil || !spec.DiskImage {
		return
	}
	dir := e.volume(w.ID, "rootfs", spec).diskDir()
	if mounted, err := disk.Mounted(dir); err == nil && mounted {
		_ = disk.Thaw(dir)
	}
}

// thawAll thaws every live workspace filesystem: an engine that stopped while
// the guard held them leaves them frozen.
func (e *Engine) thawAll(ctx context.Context) { e.setDisksReadOnly(ctx, false) }
