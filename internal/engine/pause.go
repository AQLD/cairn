package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AQLD/cairn/internal/failpoint"
	"github.com/AQLD/cairn/internal/save"
	"github.com/AQLD/cairn/internal/store"
)

// Pausing frees a workspace's memory and CPU without the caller noticing: the
// engine takes a full save, stops the sandbox, and wakes it from that save the
// next time anything needs it running. Idle workspaces are paused after a while,
// and every workspace is paused when the engine shuts down (a node drain, an
// upgrade), so their processes survive the engine going away.
//
// Waking restores memory only: the filesystem on disk is exactly the one saved
// with it, because nothing runs while the workspace is paused and writes wake
// it first. The pause's save is removed once the workspace is awake.

// Pause pauses a running workspace now. Paused and stopped workspaces are left
// as they are.
func (e *Engine) Pause(ctx context.Context, id string) (*Workspace, error) {
	if !e.cfg.Runtime.Capabilities().FullSave {
		return nil, fmt.Errorf("pause: %w (runtime %s)", ErrUnsupported, e.cfg.Runtime.Name())
	}
	l := e.lock(id)
	if err := l.lockAlone(ctx, e.cfg.ExclusiveWait, "pausing"); err != nil {
		return nil, err
	}
	defer l.Unlock()
	if err := e.pauseLocked(ctx, id); err != nil {
		return nil, err
	}
	return e.GetWorkspace(ctx, id)
}

func (e *Engine) pauseLocked(ctx context.Context, id string) error {
	w, err := e.liveWorkspace(ctx, id)
	if err != nil {
		return err
	}
	switch w.State {
	case store.Paused, store.Stopped:
		return nil
	case store.Running:
	default:
		return fmt.Errorf("%w: workspace %s is %s", ErrConflict, id, w.State)
	}
	spec, err := e.runtimeSpec(w)
	if err != nil {
		return err
	}
	if err := e.checkSpace(spec); err != nil {
		return err
	}
	saveID := newID("sv")
	now := time.Now().UTC()
	if err := e.store.CreateSave(ctx, store.Save{ID: saveID, WorkspaceID: id, Kind: string(save.Full), State: store.SavePending, CreatedAt: now, Auto: true}); err != nil {
		return err
	}
	if err := e.store.SetWorkspaceState(ctx, id, store.Pausing); err != nil {
		return err
	}
	m, err := e.writeSave(ctx, spec, saveID, save.Full, now)
	if failpoint.Crashed(err) {
		return err
	}
	if err != nil {
		_ = e.store.DeleteSave(ctx, saveID)
		_ = e.store.SetWorkspaceState(ctx, id, store.Running)
		return err
	}
	if err := e.cfg.Failpoints.Check(failpoint.PauseAfterCommit); err != nil {
		return err
	}
	defer e.kickUploads()
	if err := e.store.MarkSaveReady(ctx, saveID, m.Size()); err != nil {
		return err
	}
	if err := e.store.SetPaused(ctx, id, store.Paused, saveID); err != nil {
		return err
	}
	e.log.Info("paused workspace", "workspace", id, "save", saveID, "bytes", m.Size())
	return nil
}

// wake makes sure a paused workspace is running before something uses it.
// Anything else is left to the caller's own checks.
func (e *Engine) wake(ctx context.Context, id string) error {
	if w, err := e.store.GetWorkspace(ctx, id); err != nil || w.State != store.Paused {
		return nil
	}
	l := e.lock(id)
	if err := l.lockAlone(ctx, e.cfg.ExclusiveWait, "waking"); err != nil {
		return err
	}
	defer l.Unlock()
	return e.wakeLocked(ctx, id)
}

func (e *Engine) wakeLocked(ctx context.Context, id string) error {
	w, err := e.store.GetWorkspace(ctx, id)
	if err != nil || w.State != store.Paused {
		return err
	}
	spec, err := e.runtimeSpec(w)
	if err != nil {
		return err
	}
	if err := e.store.SetWorkspaceState(ctx, id, store.Resuming); err != nil {
		return err
	}
	if psv, err := e.store.GetSave(ctx, w.PausedSave); err == nil {
		_ = e.ensureLocal(ctx, psv) // a pause save kept only in object storage
	}
	_, verr := save.Verify(e.saveDir(w.PausedSave))
	if verr == nil {
		verr = e.cfg.Failpoints.Check(failpoint.WakeBeforeRestore)
		if failpoint.Crashed(verr) {
			return verr
		}
		if verr == nil {
			verr = e.cfg.Runtime.Restore(ctx, spec, filepath.Join(e.saveDir(w.PausedSave), save.MemoryDir))
		}
	}
	if verr != nil {
		// Its processes cannot come back; its files are all on disk. Start it
		// like after a reboot rather than leave it unusable.
		e.log.Error("could not wake workspace from its pause; starting it from its files", "workspace", id, "save", w.PausedSave, "err", verr)
		if err := e.cfg.Runtime.Start(ctx, spec); err != nil {
			_ = e.store.SetWorkspaceState(ctx, id, store.Paused)
			return err
		}
	}
	if err := e.store.SetPaused(ctx, id, store.Running, ""); err != nil {
		return err
	}
	e.dropAutoSave(ctx, w.PausedSave)
	e.touch(id)
	e.log.Info("woke workspace", "workspace", id)
	return nil
}

// useRunning wakes a paused workspace and holds its lock shared, for commands
// and writes. A pause can slip in between the wake and the lock (the idle
// pauser), so it tries again rather than answer conflict for a workspace that
// was running a moment ago.
func (e *Engine) useRunning(ctx context.Context, id string) (*wsLock, *store.Workspace, error) {
	for attempt := 0; ; attempt++ {
		if err := e.wake(ctx, id); err != nil {
			return nil, nil, err
		}
		e.touch(id)
		l := e.lock(id)
		l.RLock()
		w, err := e.runningWorkspace(ctx, id)
		if err == nil {
			return l, w, nil
		}
		l.RUnlock()
		if attempt == 3 || errors.Is(err, ErrNotFound) {
			return nil, nil, err
		}
		if cur, gerr := e.store.GetWorkspace(ctx, id); gerr != nil || cur.State != store.Paused {
			return nil, nil, err
		}
	}
}

func errAutoSave(id string) error {
	return fmt.Errorf("%w: save %s is the engine's own pause of a workspace; use the workspace (it wakes on use)", ErrConflict, id)
}

// dropAutoSave removes a pause's save once nothing needs it.
func (e *Engine) dropAutoSave(ctx context.Context, id string) {
	if id == "" {
		return
	}
	if err := e.deleteRemote(ctx, id); err != nil {
		e.log.Error("could not remove pause save from object storage", "save", id, "err", err)
	}
	if err := os.RemoveAll(e.saveDir(id)); err != nil {
		e.log.Error("could not remove pause save", "save", id, "err", err)
		return
	}
	_ = e.store.DeleteSave(ctx, id)
}

// touch records that a workspace was just used.
func (e *Engine) touch(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.activity[id] = e.cfg.Now()
}

func (e *Engine) forget(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.activity, id)
}

// idleFor is how long a workspace has gone unused. A workspace the engine has
// not seen used since it started counts from now.
func (e *Engine) idleFor(id string) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	last, ok := e.activity[id]
	if !ok {
		e.activity[id] = e.cfg.Now()
		return 0
	}
	return e.cfg.Now().Sub(last)
}

// PauseIdle pauses running workspaces unused for at least idle and with no
// command running. It returns how many it paused.
func (e *Engine) PauseIdle(ctx context.Context, idle time.Duration) (int, error) {
	if !e.cfg.Runtime.Capabilities().FullSave {
		return 0, nil
	}
	list, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return 0, err
	}
	paused := 0
	for _, w := range list {
		if w.State != store.Running || e.idleFor(w.ID) < idle {
			continue
		}
		l := e.lock(w.ID)
		if !l.TryLock() {
			continue // in use right now
		}
		if e.idleFor(w.ID) < idle { // used since we looked
			l.Unlock()
			continue
		}
		err := e.pauseLocked(ctx, w.ID)
		l.Unlock()
		switch {
		case err == nil:
			paused++
		case failpoint.Crashed(err):
			return paused, err
		default:
			e.log.Error("could not pause idle workspace", "workspace", w.ID, "err", err)
		}
	}
	return paused, nil
}

// PauseAll pauses every running workspace, ending commands still running, so
// their processes survive the engine stopping. It works on a few at a time and
// stops at ctx's deadline; workspaces it did not reach restart from their files.
func (e *Engine) PauseAll(ctx context.Context) (paused, failed int) {
	if !e.cfg.Runtime.Capabilities().FullSave {
		return 0, 0
	}
	list, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		e.log.Error("pause all: list workspaces", "err", err)
		return 0, 0
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, 4)
	)
	for _, w := range list {
		if w.State != store.Running {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			l := e.lock(id)
			l.cancelCommands()
			l.Lock()
			err := e.pauseLocked(ctx, id)
			l.Unlock()
			mu.Lock()
			defer mu.Unlock()
			if err != nil && !errors.Is(err, context.Canceled) {
				e.log.Error("could not pause workspace for shutdown", "workspace", id, "err", err)
				failed++
				return
			}
			paused++
		}(w.ID)
	}
	wg.Wait()
	return paused, failed
}

// recoverPause finishes a pause or a wake that a crash interrupted. A pause
// whose save was committed counts as paused (its processes are in the save); a
// wake that did not finish goes back to paused. It reports whether it handled w.
func (e *Engine) recoverPause(ctx context.Context, w *store.Workspace) (bool, error) {
	var saveID string
	switch {
	case w.State == store.Resuming && w.PausedSave != "":
		saveID = w.PausedSave
	case w.State == store.Pausing:
		saves, err := e.store.ListSaves(ctx, w.ID)
		if err != nil {
			return false, err
		}
		for _, sv := range saves { // newest first
			if sv.Auto && sv.State == store.SaveReady {
				saveID = sv.ID
				break
			}
		}
		if saveID == "" {
			return false, nil // never committed: back to running
		}
	default:
		return false, nil
	}
	if spec, err := e.runtimeSpec(w); err == nil {
		_ = e.cfg.Runtime.Stop(ctx, spec) // a half-restored sandbox
	}
	if err := e.store.SetPaused(ctx, w.ID, store.Paused, saveID); err != nil {
		return false, err
	}
	e.log.Info("recovered paused workspace", "workspace", w.ID, "was", w.State, "save", saveID)
	return true, nil
}
