package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/AQLD/cairn/internal/save"
	"github.com/AQLD/cairn/internal/store"
)

// PushArchive merges a tar stream into a folder of the workspace (created if
// missing): a project folder going in. Like WriteFile it wakes a paused
// workspace (its processes may hold these files) and acknowledges only once
// everything is on disk.
func (e *Engine) PushArchive(ctx context.Context, id, dir string, r io.Reader, maxBytes int64) (save.ArchiveStats, error) {
	rel, err := workspacePath(dir)
	if err != nil {
		return save.ArchiveStats{}, err
	}
	if err := e.checkDisk(ctx, id); err != nil {
		return save.ArchiveStats{}, err
	}
	l, w, err := e.useRunning(ctx, id)
	if err != nil {
		return save.ArchiveStats{}, err
	}
	defer l.RUnlock()
	spec, err := specOf(w)
	if err != nil {
		return save.ArchiveStats{}, err
	}
	root, err := os.OpenRoot(e.rootDir(id))
	if err != nil {
		return save.ArchiveStats{}, err
	}
	defer root.Close()
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return save.ArchiveStats{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	sub, err := root.OpenRoot(rel) // resolved inside the workspace, links included
	if err != nil {
		return save.ArchiveStats{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	defer sub.Close()
	if !spec.DiskImage {
		// a plain folder shares the engine's volume (see WriteFile)
		r = &readGuard{r: r, check: func() error { return e.needSpace("pushing into "+rel, 0) }}
	}
	st, err := save.ExtractTo(sub, r, maxBytes)
	if errors.Is(err, save.ErrBadArchive) {
		return st, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err == nil {
		err = syncTree(e.rootDir(id)) // acknowledged means on disk
	}
	if err == nil {
		err = syncDirsIn(root, rel)
	}
	return st, diskFull(err, spec)
}

// OpenArchive opens a folder of the workspace for PullArchive's tar stream.
// Like OpenFile it needs no running sandbox. The caller closes the root.
func (e *Engine) OpenArchive(ctx context.Context, id, dir string) (*os.Root, error) {
	rel, err := workspacePath(dir)
	if err != nil {
		return nil, err
	}
	w, err := e.liveWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.State != store.Running && w.State != store.Paused && w.State != store.Stopped {
		return nil, fmt.Errorf("%w: workspace %s is %s", ErrConflict, id, w.State)
	}
	root, err := os.OpenRoot(e.rootDir(id))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	sub, err := root.OpenRoot(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, dir)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not a folder in the workspace: %v", ErrInvalid, dir, err)
	}
	return sub, nil
}

// readGuard checks free space every 64 MiB read.
type readGuard struct {
	r     io.Reader
	check func() error
	since int64
}

func (g *readGuard) Read(p []byte) (int, error) {
	if g.since >= 64<<20 {
		if err := g.check(); err != nil {
			return 0, err
		}
		g.since = 0
	}
	n, err := g.r.Read(p)
	g.since += int64(n)
	return n, err
}
