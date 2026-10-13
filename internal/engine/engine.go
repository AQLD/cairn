// Package engine runs workspaces: their lifecycle, files, saves, resumes and
// copies, and recovery after a crash.
//
// Every state change is recorded in the store before the engine acts on it, and
// every multi-step operation is written so that a crash at any point leaves
// something recovery can finish or roll back. Open runs that recovery.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AQLD/cairn/internal/failpoint"
	"github.com/AQLD/cairn/internal/runtime"
	"github.com/AQLD/cairn/internal/save"
	"github.com/AQLD/cairn/internal/store"
)

// Errors callers can match with errors.Is. The API maps each to a status code.
var (
	ErrNotFound    = store.ErrNotFound
	ErrInvalid     = errors.New("invalid argument")
	ErrConflict    = errors.New("conflict")
	ErrUnsupported = runtime.ErrUnsupported
	ErrCorrupt     = save.ErrCorrupt
)

// MaxCopies is the most copies one request may make.
const MaxCopies = 64

type Config struct {
	DataDir    string
	Runtime    runtime.Runtime
	Logger     *slog.Logger
	Failpoints *failpoint.Set // tests only
}

type Engine struct {
	cfg   Config
	log   *slog.Logger
	store *store.Store

	mu    sync.Mutex
	locks map[string]*sync.RWMutex // per workspace
}

// Spec is the part of a workspace the caller chooses.
type Spec struct {
	Limits  runtime.Limits `json:"limits"`
	Network bool           `json:"network"`
}

// Workspace is what the engine reports about a workspace.
type Workspace struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	State      store.WorkspaceState `json:"state"`
	Limits     runtime.Limits       `json:"limits"`
	Network    bool                 `json:"network"`
	SourceSave string               `json:"source_save,omitempty"`
	CreatedAt  time.Time            `json:"created_at"`
	UpdatedAt  time.Time            `json:"updated_at"`
}

type Save struct {
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	Kind        save.Kind       `json:"kind"`
	State       store.SaveState `json:"state"`
	Size        int64           `json:"size"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Info describes the engine and what its runtime can do.
type Info struct {
	Runtime        string               `json:"runtime"`
	RuntimeVersion string               `json:"runtime_version"`
	Capabilities   runtime.Capabilities `json:"capabilities"`
	SaveFormat     int                  `json:"save_format"`
}

// Open starts the engine on a data folder and recovers from any crash.
func Open(ctx context.Context, cfg Config) (*Engine, error) {
	if cfg.DataDir == "" || cfg.Runtime == nil {
		return nil, errors.New("engine: DataDir and Runtime are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "workspaces"), filepath.Join(cfg.DataDir, "saves")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return nil, err
	}
	e := &Engine{cfg: cfg, log: cfg.Logger, store: st, locks: map[string]*sync.RWMutex{}}
	if err := e.recover(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("recover: %w", err)
	}
	return e, nil
}

func (e *Engine) Close() error { return e.store.Close() }

func (e *Engine) Info() Info {
	return Info{
		Runtime:        e.cfg.Runtime.Name(),
		RuntimeVersion: e.cfg.Runtime.Version(),
		Capabilities:   e.cfg.Runtime.Capabilities(),
		SaveFormat:     save.FormatVersion,
	}
}

// --- workspaces ---

type CreateRequest struct {
	Name    string         `json:"name"`
	Limits  runtime.Limits `json:"limits"`
	Network bool           `json:"network"`
}

func (e *Engine) CreateWorkspace(ctx context.Context, req CreateRequest) (*Workspace, error) {
	if req.Limits.CPUs < 0 || req.Limits.MemoryMB < 0 || req.Limits.DiskMB < 0 {
		return nil, fmt.Errorf("%w: limits cannot be negative", ErrInvalid)
	}
	if !e.cfg.Runtime.Capabilities().NetworkControl {
		// The setting is recorded either way; Info reports that it is not enforced.
		e.log.Warn("runtime cannot control networking; workspace network setting is not enforced", "runtime", e.cfg.Runtime.Name())
	}
	id := newID("ws")
	name := req.Name
	if name == "" {
		name = id
	}
	spec := Spec{Limits: req.Limits, Network: req.Network}
	w, err := e.insertWorkspace(ctx, id, name, spec, "")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(e.rootDir(id), 0o755); err != nil {
		return nil, err
	}
	if err := e.cfg.Failpoints.Check(failpoint.CreateBeforeReady); err != nil {
		return nil, err
	}
	if err := e.start(ctx, w); err != nil {
		return nil, err
	}
	return e.GetWorkspace(ctx, id)
}

func (e *Engine) insertWorkspace(ctx context.Context, id, name string, spec Spec, sourceSave string) (*store.Workspace, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	w := store.Workspace{ID: id, Name: name, State: store.Creating, Spec: raw, SourceSave: sourceSave, CreatedAt: now, UpdatedAt: now}
	if err := e.store.CreateWorkspace(ctx, w); err != nil {
		return nil, err
	}
	return &w, nil
}

func (e *Engine) start(ctx context.Context, w *store.Workspace) error {
	spec, err := e.runtimeSpec(w)
	if err != nil {
		return err
	}
	if err := e.cfg.Runtime.Start(ctx, spec); err != nil {
		return err
	}
	return e.store.SetWorkspaceState(ctx, w.ID, store.Running)
}

func (e *Engine) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	w, err := e.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.State == store.Deleted {
		return nil, ErrNotFound
	}
	return toWorkspace(w)
}

func (e *Engine) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	ws, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Workspace, 0, len(ws))
	for i := range ws {
		w, err := toWorkspace(&ws[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, nil
}

// DeleteWorkspace stops a workspace and removes its filesystem. Its saves are
// kept, so it can still be copied from.
func (e *Engine) DeleteWorkspace(ctx context.Context, id string) error {
	l := e.lock(id)
	l.Lock()
	defer l.Unlock()
	w, err := e.liveWorkspace(ctx, id)
	if err != nil {
		return err
	}
	spec, err := e.runtimeSpec(w)
	if err != nil {
		return err
	}
	if err := e.cfg.Runtime.Stop(ctx, spec); err != nil {
		return err
	}
	if err := e.store.SetWorkspaceState(ctx, id, store.Deleted); err != nil {
		return err
	}
	return os.RemoveAll(e.workspaceDir(id))
}

// --- commands and files ---

type ExecRequest = runtime.ExecRequest
type ExecResult = runtime.ExecResult

func (e *Engine) Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error) {
	if len(req.Argv) == 0 {
		return ExecResult{}, fmt.Errorf("%w: command is empty", ErrInvalid)
	}
	l := e.lock(id)
	l.RLock()
	defer l.RUnlock()
	w, err := e.runningWorkspace(ctx, id)
	if err != nil {
		return ExecResult{}, err
	}
	spec, err := e.runtimeSpec(w)
	if err != nil {
		return ExecResult{}, err
	}
	return e.cfg.Runtime.Exec(ctx, spec, req)
}

// WriteFile writes a file inside the workspace, creating parent folders.
func (e *Engine) WriteFile(ctx context.Context, id, name string, r io.Reader, mode os.FileMode) error {
	rel, err := workspacePath(name)
	if err != nil {
		return err
	}
	l := e.lock(id)
	l.RLock()
	defer l.RUnlock()
	if _, err := e.runningWorkspace(ctx, id); err != nil {
		return err
	}
	root, err := os.OpenRoot(e.rootDir(id))
	if err != nil {
		return err
	}
	defer root.Close()
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if mode == 0 {
		mode = 0o644
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// OpenFile opens a file inside the workspace for reading. The caller closes it.
func (e *Engine) OpenFile(ctx context.Context, id, name string) (*os.File, os.FileInfo, error) {
	rel, err := workspacePath(name)
	if err != nil {
		return nil, nil, err
	}
	if _, err := e.runningWorkspace(ctx, id); err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(e.rootDir(id))
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if info.IsDir() {
		f.Close()
		return nil, nil, fmt.Errorf("%w: %s is a folder", ErrInvalid, name)
	}
	return f, info, nil
}

// workspacePath turns a caller's path into a clean path relative to the
// workspace root. os.Root does the real enforcement, including symlinks.
func workspacePath(name string) (string, error) {
	p := path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
	rel := strings.TrimPrefix(p, "/")
	if rel == "" {
		return "", fmt.Errorf("%w: path is empty", ErrInvalid)
	}
	return rel, nil
}

// --- saves ---

// Save writes a quick (filesystem) or full (filesystem, memory and processes)
// save. A full save stops the workspace; resume it to continue.
func (e *Engine) Save(ctx context.Context, id string, kind save.Kind) (*Save, error) {
	if kind == "" {
		kind = save.Quick
	}
	if kind != save.Quick && kind != save.Full {
		return nil, fmt.Errorf("%w: unknown save kind %q", ErrInvalid, kind)
	}
	if kind == save.Full && !e.cfg.Runtime.Capabilities().FullSave {
		return nil, fmt.Errorf("full save: %w (runtime %s)", ErrUnsupported, e.cfg.Runtime.Name())
	}
	l := e.lock(id)
	if kind == save.Full {
		l.Lock() // nothing may run while memory is captured
		defer l.Unlock()
	} else {
		l.RLock() // quick saves let commands keep running
		defer l.RUnlock()
	}
	w, err := e.runningWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	spec, err := e.runtimeSpec(w)
	if err != nil {
		return nil, err
	}

	saveID := newID("sv")
	now := time.Now().UTC()
	if err := e.store.CreateSave(ctx, store.Save{ID: saveID, WorkspaceID: id, Kind: string(kind), State: store.SavePending, CreatedAt: now}); err != nil {
		return nil, err
	}
	if kind == save.Full {
		if err := e.store.SetWorkspaceState(ctx, id, store.Saving); err != nil {
			return nil, err
		}
	}

	m, err := e.writeSave(ctx, spec, saveID, kind, now)
	if failpoint.Crashed(err) {
		return nil, err
	}
	if err != nil {
		_ = e.store.DeleteSave(ctx, saveID)
		_ = e.store.SetWorkspaceState(ctx, id, store.Running)
		return nil, err
	}
	if err := e.cfg.Failpoints.Check(failpoint.SaveAfterCommit); err != nil {
		return nil, err
	}
	if err := e.store.MarkSaveReady(ctx, saveID, m.Size()); err != nil {
		return nil, err
	}
	if kind == save.Full {
		if err := e.store.SetWorkspaceState(ctx, id, store.Stopped); err != nil {
			return nil, err
		}
	}
	return e.GetSave(ctx, saveID)
}

func (e *Engine) writeSave(ctx context.Context, spec runtime.Spec, saveID string, kind save.Kind, now time.Time) (*save.Manifest, error) {
	b, err := save.NewBuilder(e.savesDir(), save.Manifest{
		SaveID:         saveID,
		WorkspaceID:    spec.ID,
		Kind:           kind,
		CreatedAt:      now,
		Runtime:        e.cfg.Runtime.Name(),
		RuntimeVersion: e.cfg.Runtime.Version(),
	})
	if err != nil {
		return nil, err
	}
	crashed := false
	defer func() {
		if !crashed {
			b.Abort() // no-op after Commit
		}
	}()

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(save.TarDir(spec.RootDir, pw)) }()
	if err := b.WriteFile(save.RootfsFile, pr); err != nil {
		pr.CloseWithError(err)
		return nil, fmt.Errorf("save filesystem: %w", err)
	}
	if kind == save.Full {
		if err := e.cfg.Runtime.Checkpoint(ctx, spec, b.Dir()); err != nil {
			return nil, fmt.Errorf("save memory: %w", err)
		}
		if err := b.AddFile(save.MemoryFile); err != nil {
			return nil, err
		}
	}
	if err := e.cfg.Failpoints.Check(failpoint.SaveBeforeCommit); err != nil {
		crashed = failpoint.Crashed(err)
		return nil, err
	}
	return b.Commit()
}

func (e *Engine) GetSave(ctx context.Context, id string) (*Save, error) {
	sv, err := e.store.GetSave(ctx, id)
	if err != nil {
		return nil, err
	}
	if sv.State == store.SavePending {
		return nil, ErrNotFound
	}
	return toSave(sv), nil
}

func (e *Engine) ListSaves(ctx context.Context, workspaceID string) ([]Save, error) {
	if _, err := e.store.GetWorkspace(ctx, workspaceID); err != nil {
		return nil, err
	}
	svs, err := e.store.ListSaves(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]Save, 0, len(svs))
	for i := range svs {
		out = append(out, *toSave(&svs[i]))
	}
	return out, nil
}

// readySave loads a save and verifies every byte before anything uses it.
func (e *Engine) readySave(ctx context.Context, saveID string) (*store.Save, *save.Manifest, error) {
	sv, err := e.store.GetSave(ctx, saveID)
	if err != nil {
		return nil, nil, err
	}
	if sv.State != store.SaveReady {
		return nil, nil, fmt.Errorf("%w: save %s is %s", ErrConflict, saveID, sv.State)
	}
	m, err := save.Verify(e.saveDir(saveID))
	if err != nil {
		if errors.Is(err, save.ErrCorrupt) {
			e.log.Error("save failed verification", "save", saveID, "err", err)
		}
		return nil, nil, err
	}
	if m.Kind == save.Full && m.Runtime != e.cfg.Runtime.Name() {
		return nil, nil, fmt.Errorf("%w: full save made by runtime %q cannot restore on %q", ErrConflict, m.Runtime, e.cfg.Runtime.Name())
	}
	return sv, m, nil
}

// Resume restores a workspace to one of its saves: the filesystem, and for a
// full save its memory and processes. Anything written since the save is lost.
func (e *Engine) Resume(ctx context.Context, saveID string) (*Workspace, error) {
	sv, m, err := e.readySave(ctx, saveID)
	if err != nil {
		return nil, err
	}
	id := sv.WorkspaceID
	l := e.lock(id)
	l.Lock()
	defer l.Unlock()
	w, err := e.liveWorkspace(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: workspace %s was deleted; copy the save instead", ErrConflict, id)
		}
		return nil, err
	}
	spec, err := e.runtimeSpec(w)
	if err != nil {
		return nil, err
	}
	if err := e.store.SetWorkspaceState(ctx, id, store.Resuming); err != nil {
		return nil, err
	}
	if err := e.cfg.Runtime.Stop(ctx, spec); err != nil {
		return nil, err
	}
	if err := e.restoreInto(ctx, spec, saveID, m, failpoint.ResumeBeforeSwap); err != nil {
		if !failpoint.Crashed(err) {
			_ = e.store.SetWorkspaceState(ctx, id, store.Running)
		}
		return nil, err
	}
	if err := e.cfg.Failpoints.Check(failpoint.ResumeAfterSwap); err != nil {
		return nil, err
	}
	if err := e.store.SetWorkspaceState(ctx, id, store.Running); err != nil {
		return nil, err
	}
	return e.GetWorkspace(ctx, id)
}

// restoreInto extracts a save's filesystem next to the workspace's current one,
// swaps it in, then starts the workspace (restoring memory for a full save).
// A crash at any point leaves either the old or the new filesystem in place;
// recovery finishes the swap or rolls it back.
func (e *Engine) restoreInto(ctx context.Context, spec runtime.Spec, saveID string, m *save.Manifest, beforeSwap string) error {
	dir := e.workspaceDir(spec.ID)
	next, prev := filepath.Join(dir, "rootfs.next"), filepath.Join(dir, "rootfs.prev")
	_ = os.RemoveAll(next)
	f, err := os.Open(filepath.Join(e.saveDir(saveID), save.RootfsFile))
	if err != nil {
		return err
	}
	err = save.UntarDir(f, next)
	f.Close()
	if err != nil {
		_ = os.RemoveAll(next)
		return err
	}
	if err := e.cfg.Failpoints.Check(beforeSwap); err != nil {
		return err
	}
	if _, err := os.Stat(spec.RootDir); err == nil {
		if err := os.Rename(spec.RootDir, prev); err != nil {
			return err
		}
	}
	if err := os.Rename(next, spec.RootDir); err != nil {
		return err
	}
	_ = os.RemoveAll(prev)

	if m.Kind == save.Full {
		return e.cfg.Runtime.Restore(ctx, spec, e.saveDir(saveID))
	}
	return e.cfg.Runtime.Start(ctx, spec)
}

// Copy makes count new workspaces from one save. Each copy has its own ID and
// identity; the original workspace is untouched.
func (e *Engine) Copy(ctx context.Context, saveID string, count int) ([]Workspace, error) {
	if count < 1 || count > MaxCopies {
		return nil, fmt.Errorf("%w: count must be between 1 and %d", ErrInvalid, MaxCopies)
	}
	sv, m, err := e.readySave(ctx, saveID)
	if err != nil {
		return nil, err
	}
	src, err := e.store.GetWorkspace(ctx, sv.WorkspaceID)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(src.Spec, &spec); err != nil {
		return nil, err
	}

	out := make([]Workspace, 0, count)
	for i := 0; i < count; i++ {
		id := newID("ws")
		w, err := e.insertWorkspace(ctx, id, fmt.Sprintf("%s-copy-%d", src.Name, i+1), spec, saveID)
		if err != nil {
			return out, err
		}
		rs, err := e.runtimeSpec(w)
		if err != nil {
			return out, err
		}
		if err := os.MkdirAll(e.workspaceDir(id), 0o700); err != nil {
			return out, err
		}
		if err := e.restoreInto(ctx, rs, saveID, m, failpoint.CopyBeforeRunning); err != nil {
			return out, err
		}
		if err := e.store.SetWorkspaceState(ctx, id, store.Running); err != nil {
			return out, err
		}
		cw, err := e.GetWorkspace(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, *cw)
	}
	return out, nil
}

// --- recovery ---

// recover brings the data folder back to a consistent state after a crash:
// half-written saves are discarded, interrupted resumes are finished or rolled
// back, and every live workspace is started.
func (e *Engine) recover(ctx context.Context) error {
	if n, err := save.CleanupTemp(e.savesDir()); err != nil {
		return err
	} else if n > 0 {
		e.log.Info("removed half-written saves", "count", n)
	}

	// Saves: pending ones were never published; ready ones must still exist.
	saves, err := e.store.AllSaves(ctx)
	if err != nil {
		return err
	}
	for _, sv := range saves {
		_, statErr := os.Stat(filepath.Join(e.saveDir(sv.ID), save.ManifestFile))
		switch {
		case sv.State == store.SavePending && statErr == nil:
			// committed to disk, crashed before it was marked ready: publish it if it verifies
			if m, err := save.Verify(e.saveDir(sv.ID)); err == nil {
				if err := e.store.MarkSaveReady(ctx, sv.ID, m.Size()); err != nil {
					return err
				}
				e.log.Info("published save committed before a crash", "save", sv.ID)
				continue
			}
			fallthrough
		case sv.State == store.SavePending:
			_ = os.RemoveAll(e.saveDir(sv.ID))
			if err := e.store.DeleteSave(ctx, sv.ID); err != nil {
				return err
			}
		case sv.State == store.SaveReady && statErr != nil:
			e.log.Error("save folder missing", "save", sv.ID)
			if err := e.store.SetSaveState(ctx, sv.ID, store.SaveMissing); err != nil {
				return err
			}
		}
	}

	// Workspaces interrupted mid-operation.
	ws, err := e.store.TransitionalWorkspaces(ctx)
	if err != nil {
		return err
	}
	for i := range ws {
		w := &ws[i]
		if w.State == store.Creating && w.SourceSave != "" {
			// a copy interrupted before it was handed out: the caller never got it
			if err := e.store.SetWorkspaceState(ctx, w.ID, store.Deleted); err != nil {
				return err
			}
			_ = os.RemoveAll(e.workspaceDir(w.ID))
			e.log.Info("discarded interrupted copy", "workspace", w.ID)
			continue
		}
		if err := e.finishSwap(w.ID); err != nil {
			return err
		}
		if err := os.MkdirAll(e.rootDir(w.ID), 0o755); err != nil {
			return err
		}
		e.log.Info("recovered interrupted workspace", "workspace", w.ID, "was", w.State)
		if err := e.store.SetWorkspaceState(ctx, w.ID, store.Running); err != nil {
			return err
		}
	}

	// Start everything that should be running.
	live, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	for i := range live {
		if live[i].State != store.Running {
			continue
		}
		spec, err := e.runtimeSpec(&live[i])
		if err != nil {
			return err
		}
		if err := e.cfg.Runtime.Start(ctx, spec); err != nil {
			e.log.Error("could not start workspace after recovery", "workspace", live[i].ID, "err", err)
		}
	}
	return nil
}

// finishSwap completes or rolls back a filesystem swap a crash interrupted.
func (e *Engine) finishSwap(id string) error {
	dir := e.workspaceDir(id)
	root, next, prev := e.rootDir(id), filepath.Join(dir, "rootfs.next"), filepath.Join(dir, "rootfs.prev")
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		// crashed between the two renames: the new filesystem is complete, finish
		if _, err := os.Stat(next); err == nil {
			if err := os.Rename(next, root); err != nil {
				return err
			}
		} else if _, err := os.Stat(prev); err == nil {
			if err := os.Rename(prev, root); err != nil {
				return err
			}
		}
	}
	_ = os.RemoveAll(next) // never swapped in: the old filesystem stays
	_ = os.RemoveAll(prev)
	return nil
}

// --- helpers ---

func (e *Engine) lock(id string) *sync.RWMutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.locks[id]
	if !ok {
		l = &sync.RWMutex{}
		e.locks[id] = l
	}
	return l
}

func (e *Engine) liveWorkspace(ctx context.Context, id string) (*store.Workspace, error) {
	w, err := e.store.GetWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.State == store.Deleted {
		return nil, ErrNotFound
	}
	return w, nil
}

func (e *Engine) runningWorkspace(ctx context.Context, id string) (*store.Workspace, error) {
	w, err := e.liveWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.State != store.Running {
		return nil, fmt.Errorf("%w: workspace %s is %s", ErrConflict, id, w.State)
	}
	return w, nil
}

func (e *Engine) runtimeSpec(w *store.Workspace) (runtime.Spec, error) {
	var spec Spec
	if len(w.Spec) > 0 {
		if err := json.Unmarshal(w.Spec, &spec); err != nil {
			return runtime.Spec{}, err
		}
	}
	return runtime.Spec{ID: w.ID, Hostname: w.ID, RootDir: e.rootDir(w.ID), Limits: spec.Limits, Network: spec.Network}, nil
}

func (e *Engine) workspaceDir(id string) string { return filepath.Join(e.cfg.DataDir, "workspaces", id) }
func (e *Engine) rootDir(id string) string      { return filepath.Join(e.workspaceDir(id), "rootfs") }
func (e *Engine) savesDir() string              { return filepath.Join(e.cfg.DataDir, "saves") }
func (e *Engine) saveDir(id string) string      { return filepath.Join(e.savesDir(), id) }

// SaveDir is where a save lives on disk; tests use it to inject corruption.
func (e *Engine) SaveDir(id string) string { return e.saveDir(id) }

func toWorkspace(w *store.Workspace) (*Workspace, error) {
	var spec Spec
	if len(w.Spec) > 0 {
		if err := json.Unmarshal(w.Spec, &spec); err != nil {
			return nil, err
		}
	}
	return &Workspace{
		ID: w.ID, Name: w.Name, State: w.State, Limits: spec.Limits, Network: spec.Network,
		SourceSave: w.SourceSave, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}, nil
}

func toSave(sv *store.Save) *Save {
	return &Save{ID: sv.ID, WorkspaceID: sv.WorkspaceID, Kind: save.Kind(sv.Kind), State: sv.State, Size: sv.Size, CreatedAt: sv.CreatedAt}
}

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
