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
	"syscall"
	"time"

	"github.com/AQLD/cairn/internal/disk"
	"github.com/AQLD/cairn/internal/failpoint"
	"github.com/AQLD/cairn/internal/objstore"
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
	// ErrInsufficientStorage: the save would not fit (checked up front) or the
	// disk filled while writing it. Nothing is left half written.
	ErrInsufficientStorage = errors.New("insufficient storage")
	// ErrBusy: a limit on concurrent work was reached; retry shortly.
	ErrBusy = errors.New("too many requests")
)

// DefaultMinFreeBytes is the free space a save must leave on the disk.
const DefaultMinFreeBytes = 512 << 20

// MaxCopies is the most copies one request may make.
const MaxCopies = 64

// DefaultRetention is how long a deleted save can be recovered before it is purged.
const DefaultRetention = 7 * 24 * time.Hour

type Config struct {
	DataDir    string
	Runtime    runtime.Runtime
	Logger     *slog.Logger
	Retention  time.Duration    // how long deleted saves are kept; DefaultRetention if zero
	Now        func() time.Time // clock; time.Now if nil (tests move it forward)
	Failpoints *failpoint.Set   // tests only
	// MinFreeBytes is the free space a save must leave behind; a save that would
	// go below it is refused before anything is written. DefaultMinFreeBytes if 0.
	MinFreeBytes int64
	// ExclusiveWait bounds how long a full save or resume waits for running
	// commands (default DefaultExclusiveWait).
	ExclusiveWait time.Duration
	// DefaultTemplate is used when a create request names none. Runtimes that
	// boot a real sandbox (runsc) need one: an empty filesystem has no shell.
	DefaultTemplate string
	// DiskMB is the disk size of a workspace created without one
	// (DefaultDiskMB if 0). Sizes are enforced with a disk image per workspace
	// where the machine supports them (Linux, root, mkfs.ext4, loop devices).
	DiskMB int64
	// NoDiskImages keeps new workspaces in plain folders, with no disk limit.
	NoDiskImages bool
	// MaxCommands is how many commands one workspace may run at once
	// (DefaultMaxCommands if 0); more are refused with ErrBusy.
	MaxCommands int
	// MaxOps is how many heavy operations run at once (DefaultMaxOps if 0);
	// more wait for a slot.
	MaxOps int
	// Objects, if set, keeps a copy of every save in object storage, so saves
	// outlive this server (see remote.go).
	Objects objstore.Store
	// RestoreState rebuilds the state database from its last backup and the
	// data folder (see state.go) if it is missing or damaged; a healthy one is
	// left alone, so the setting is safe to leave on.
	RestoreState bool
}

type Engine struct {
	cfg   Config
	log   *slog.Logger
	store *store.Store

	mu       sync.Mutex
	diskErr  error                // why new workspaces get no disk image; nil if they do
	locks    map[string]*wsLock   // per workspace
	activity map[string]time.Time // last use per workspace, for pausing idle ones
	ops      chan struct{}        // slots for heavy operations
	running  sync.Map             // workspace/session -> struct{}: sessions running a cell

	uploadKick   chan struct{}      // wakes the uploader
	stopUploads  context.CancelFunc // ends it
	uploaderDone chan struct{}
}

// Spec is the part of a workspace the caller chooses.
type Spec struct {
	Limits   runtime.Limits `json:"limits"`
	Network  bool           `json:"network"`
	Template string         `json:"template,omitempty"`
	// DiskImage: the files live in an image of Limits.DiskMB (see volume).
	// Decided at creation; workspaces made before disk limits have none.
	DiskImage bool `json:"disk_image,omitempty"`
}

// Workspace is what the engine reports about a workspace.
type Workspace struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	State      store.WorkspaceState `json:"state"`
	Limits     runtime.Limits       `json:"limits"`
	Network    bool                 `json:"network"`
	Template   string               `json:"template,omitempty"`
	SourceSave string               `json:"source_save,omitempty"`
	BaseSave   string               `json:"base_save,omitempty"` // the save its state comes from: last saved, resumed or copied
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
	DeletedAt   *time.Time      `json:"deleted_at,omitempty"`
	PurgeAfter  *time.Time      `json:"purge_after,omitempty"` // a deleted save can be undeleted until then
	Auto        bool            `json:"auto,omitempty"`        // made by the engine to pause the workspace
	Uploaded    bool            `json:"uploaded,omitempty"`    // a copy is in object storage
	// Parent is the save the workspace's state came from when this one was
	// made (the one it was last saved, resumed or copied from): a workspace's
	// saves form a tree, and rewinding then saving starts a branch.
	Parent string `json:"parent,omitempty"`
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
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultRetention
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ExclusiveWait <= 0 {
		cfg.ExclusiveWait = DefaultExclusiveWait
	}
	if cfg.MinFreeBytes <= 0 {
		cfg.MinFreeBytes = DefaultMinFreeBytes
	}
	if cfg.DiskMB == 0 {
		cfg.DiskMB = DefaultDiskMB
	}
	if cfg.MaxCommands <= 0 {
		cfg.MaxCommands = DefaultMaxCommands
	}
	if cfg.MaxOps <= 0 {
		cfg.MaxOps = DefaultMaxOps()
	}
	if cfg.DiskMB < disk.MinSizeMB {
		return nil, fmt.Errorf("engine: disk size must be at least %d MB", disk.MinSizeMB)
	}
	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "workspaces"), filepath.Join(cfg.DataDir, "saves")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	e := &Engine{cfg: cfg, log: cfg.Logger}
	restore := false
	if cfg.RestoreState {
		var err error
		if restore, err = e.stateNeedsRestore(); err != nil {
			return nil, err
		}
	}
	if restore {
		if err := e.restoreStateFiles(); err != nil {
			return nil, fmt.Errorf("restore state: %w", err)
		}
	}
	st, err := store.Open(e.statePath())
	if errors.Is(err, store.ErrDamaged) {
		return nil, e.damagedState(err)
	}
	if err != nil {
		return nil, err
	}
	e = &Engine{cfg: cfg, log: cfg.Logger, store: st, locks: map[string]*wsLock{}, activity: map[string]time.Time{}, ops: make(chan struct{}, cfg.MaxOps)}
	if cfg.NoDiskImages {
		e.diskErr = errors.New("disk images switched off")
	} else {
		e.diskErr = disk.Supported()
	}
	if e.diskErr != nil {
		e.log.Warn("workspace disk sizes are not enforced", "reason", e.diskErr.Error())
	}
	if restore {
		if err := e.reconcile(ctx); err != nil {
			st.Close()
			return nil, fmt.Errorf("restore state: %w", err)
		}
		e.log.Info("state database rebuilt from its backup and the data folder")
	}
	if err := e.recover(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("recover: %w", err)
	}
	if _, err := e.PurgeExpired(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("purge: %w", err)
	}
	if e.remoteOn() {
		// learn saves made elsewhere; a store that is down only delays this
		if err := e.importRemote(ctx); err != nil {
			e.log.Error("could not read saves from object storage", "store", cfg.Objects.String(), "err", err)
		}
		uctx, cancel := context.WithCancel(context.Background())
		e.uploadKick, e.stopUploads, e.uploaderDone = make(chan struct{}, 1), cancel, make(chan struct{})
		go e.uploader(uctx)
	}
	return e, nil
}

// Close closes the state store. Sandboxes keep running: they belong to the
// workspaces, not to this process, and the next Open picks them up again.
func (e *Engine) Close() error {
	if e.stopUploads != nil {
		e.stopUploads()
		<-e.uploaderDone
	}
	return e.store.Close()
}

// StopAll stops every workspace's sandbox, for a host being drained or a test
// cleaning up. Workspaces stay recorded as running; the next Open starts them.
func (e *Engine) StopAll(ctx context.Context) error {
	ws, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	var first error
	for i := range ws {
		spec, err := e.runtimeSpec(&ws[i])
		if err == nil {
			err = e.cfg.Runtime.Stop(ctx, spec)
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Counts reports workspaces and saves by state, for metrics.
func (e *Engine) Counts(ctx context.Context) (*store.Counts, error) { return e.store.Counts(ctx) }

func (e *Engine) Info() Info {
	caps := e.cfg.Runtime.Capabilities()
	caps.DiskLimits = e.diskErr == nil // enforced by the engine, whatever the runtime
	return Info{
		Runtime:        e.cfg.Runtime.Name(),
		RuntimeVersion: e.cfg.Runtime.Version(),
		Capabilities:   caps,
		SaveFormat:     save.FormatVersion,
	}
}

// --- workspaces ---

type CreateRequest struct {
	Name     string         `json:"name"`
	Limits   runtime.Limits `json:"limits"`
	Network  bool           `json:"network"`
	Template string         `json:"template,omitempty"`
	// RequestKey makes the request idempotent: a retry with the same key
	// returns the workspace the first request created instead of a new one.
	RequestKey string `json:"-"`
}

func (e *Engine) CreateWorkspace(ctx context.Context, req CreateRequest) (*Workspace, error) {
	if req.Limits.CPUs < 0 || req.Limits.MemoryMB < 0 || req.Limits.DiskMB < 0 {
		return nil, fmt.Errorf("%w: limits cannot be negative", ErrInvalid)
	}
	if req.Limits.DiskMB != 0 && req.Limits.DiskMB < disk.MinSizeMB {
		return nil, fmt.Errorf("%w: disk_mb must be at least %d", ErrInvalid, disk.MinSizeMB)
	}
	if req.Limits.DiskMB != 0 && e.diskErr != nil {
		e.log.Warn("workspace disk size is not enforced", "reason", e.diskErr.Error())
	}
	if !e.cfg.Runtime.Capabilities().NetworkControl {
		// The setting is recorded either way; Info reports that it is not enforced.
		e.log.Warn("runtime cannot control networking; workspace network setting is not enforced", "runtime", e.cfg.Runtime.Name())
	}
	if req.RequestKey != "" {
		if w, err := e.workspaceForKey(ctx, req.RequestKey, ""); err != nil || w != nil {
			return w, err
		}
	}
	template := req.Template
	if template == "" {
		template = e.cfg.DefaultTemplate
	}
	var templateSize int64
	if template != "" {
		t, err := e.GetTemplate(ctx, template)
		if err != nil {
			return nil, err
		}
		templateSize = t.Size
	}
	if err := e.needSpace("the workspace", templateSize); err != nil {
		return nil, err
	}
	release, err := e.heavy(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	id := newID("ws")
	name := req.Name
	if name == "" {
		name = id
	}
	spec := Spec{Limits: req.Limits, Network: req.Network, Template: template}
	e.diskFor(&spec)
	w, err := e.insertWorkspace(ctx, id, name, spec, "", req.RequestKey)
	if errors.Is(err, store.ErrDuplicateKey) {
		return e.workspaceForKey(ctx, req.RequestKey, "") // lost a race with the same key
	}
	if err != nil {
		return nil, err
	}
	if err := e.createRootfs(id, spec); err != nil {
		_ = e.store.SetWorkspaceState(ctx, id, store.Deleted)
		_ = e.store.ReleaseWorkspaceRequestKey(ctx, id)
		_ = e.removeWorkspaceDir(id, spec)
		return nil, noSpace(err)
	}
	if err := e.cfg.Failpoints.Check(failpoint.CreateBeforeReady); err != nil {
		return nil, err
	}
	if err := e.start(ctx, w); err != nil {
		return nil, err
	}
	return e.GetWorkspace(ctx, id)
}

// workspaceForKey returns the workspace a request key already created, nil if
// the key is unused, or an error if the key belongs to a different request or
// that request is still running. sourceSave must match for copies.
func (e *Engine) workspaceForKey(ctx context.Context, key, sourceSave string) (*Workspace, error) {
	w, err := e.store.WorkspaceByRequestKey(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case w.SourceSave != sourceSave:
		return nil, fmt.Errorf("%w: idempotency key was already used for a different request", ErrConflict)
	case w.State == store.Creating:
		return nil, fmt.Errorf("%w: a request with this idempotency key is still in progress", ErrConflict)
	case w.State == store.Deleted:
		return nil, fmt.Errorf("%w: the workspace this idempotency key created has been deleted", ErrConflict)
	}
	return toWorkspace(w)
}

func (e *Engine) insertWorkspace(ctx context.Context, id, name string, spec Spec, sourceSave, requestKey string) (*store.Workspace, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	w := store.Workspace{ID: id, Name: name, State: store.Creating, Spec: raw, SourceSave: sourceSave, BaseSave: sourceSave, RequestKey: requestKey, CreatedAt: now, UpdatedAt: now}
	if err := e.store.CreateWorkspace(ctx, w); err != nil {
		return nil, err
	}
	if err := e.writeRecord(&w); err != nil {
		return nil, noSpace(err)
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
	l.cancelCommands() // deleting ends whatever runs; it does not wait for it
	l.Lock()
	defer l.Unlock()
	w, err := e.liveWorkspace(ctx, id)
	if err != nil {
		return err
	}
	defer e.dropAutoSave(ctx, w.PausedSave)
	defer e.forget(id)
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
	ws, err := specOf(w)
	if err != nil {
		return err
	}
	return e.removeWorkspaceDir(id, ws)
}

// --- commands and files ---

type ExecRequest = runtime.ExecRequest
type ExecResult = runtime.ExecResult

func (e *Engine) Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error) {
	if len(req.Argv) == 0 {
		return ExecResult{}, fmt.Errorf("%w: command is empty", ErrInvalid)
	}
	if err := e.checkDisk(ctx, id); err != nil {
		return ExecResult{}, err
	}
	l, w, err := e.useRunning(ctx, id)
	if err != nil {
		return ExecResult{}, err
	}
	defer l.RUnlock()
	defer e.touch(id)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done, err := l.command(cancel, e.cfg.MaxCommands)
	if err != nil {
		return ExecResult{}, err
	}
	defer done()
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
	if err := e.checkDisk(ctx, id); err != nil {
		return err
	}
	// a paused workspace's processes may hold this file: wake them first
	l, w, err := e.useRunning(ctx, id)
	if err != nil {
		return err
	}
	defer l.RUnlock()
	spec, err := specOf(w)
	if err != nil {
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
	var dst io.Writer = f
	if !spec.DiskImage {
		// A plain folder shares the engine's volume: stop before the upload eats
		// the headroom saves and the state database need. (A disk image is
		// bounded by its own size.)
		dst = &spaceGuard{w: f, check: func() error { return e.needSpace("writing "+rel, 0) }}
	}
	_, err = io.Copy(dst, r)
	if errors.Is(err, ErrInsufficientStorage) {
		f.Close()
		_ = root.Remove(rel) // give the space back
		return err
	}
	if err == nil {
		err = fsync(f) // acknowledged means on disk: it survives a power cut
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = syncDirsIn(root, path.Dir(rel)) // and so do the entries naming it
	}
	return diskFull(err, spec)
}

// spaceGuard checks free space every 64 MiB written.
type spaceGuard struct {
	w     io.Writer
	check func() error
	since int64
}

func (g *spaceGuard) Write(p []byte) (int, error) {
	if g.since >= 64<<20 {
		if err := g.check(); err != nil {
			return 0, err
		}
		g.since = 0
	}
	n, err := g.w.Write(p)
	g.since += int64(n)
	return n, err
}

// syncDirsIn syncs dir and every folder above it inside root, so a file just
// created there (and any folder created for it) is durably named.
func syncDirsIn(root *os.Root, dir string) error {
	for {
		d, err := root.Open(dir)
		if err != nil {
			return err
		}
		err = fsync(d)
		d.Close()
		if err != nil || dir == "." {
			return err
		}
		dir = path.Dir(dir)
	}
}

// OpenFile opens a file inside the workspace for reading. The caller closes it.
func (e *Engine) OpenFile(ctx context.Context, id, name string) (*os.File, os.FileInfo, error) {
	rel, err := workspacePath(name)
	if err != nil {
		return nil, nil, err
	}
	// Reading needs no running sandbox: a paused or stopped workspace's files
	// are on disk as saved, so a person can look at a waiting agent's work.
	w, err := e.liveWorkspace(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if w.State != store.Running && w.State != store.Paused && w.State != store.Stopped {
		return nil, nil, fmt.Errorf("%w: workspace %s is %s", ErrConflict, id, w.State)
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
// requestKey, if set, makes the save idempotent: a retry returns the save the
// first request made, even if the engine crashed before replying.
func (e *Engine) Save(ctx context.Context, id string, kind save.Kind, requestKey string) (*Save, error) {
	if kind == "" {
		kind = save.Quick
	}
	if requestKey != "" {
		if sv, err := e.saveForKey(ctx, requestKey, id); err != nil || sv != nil {
			return sv, err
		}
	}
	if kind != save.Quick && kind != save.Full {
		return nil, fmt.Errorf("%w: unknown save kind %q", ErrInvalid, kind)
	}
	if kind == save.Full && !e.cfg.Runtime.Capabilities().FullSave {
		return nil, fmt.Errorf("full save: %w (runtime %s)", ErrUnsupported, e.cfg.Runtime.Name())
	}
	release, err := e.heavy(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := e.wake(ctx, id); err != nil {
		return nil, err
	}
	e.touch(id)
	l := e.lock(id)
	if kind == save.Full {
		// nothing may run while memory is captured
		if err := l.lockAlone(ctx, e.cfg.ExclusiveWait, "a full save"); err != nil {
			return nil, err
		}
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

	if err := e.checkSpace(spec); err != nil {
		return nil, err
	}

	saveID := newID("sv")
	now := time.Now().UTC()
	if err := e.store.CreateSave(ctx, store.Save{ID: saveID, WorkspaceID: id, Kind: string(kind), State: store.SavePending, CreatedAt: now, RequestKey: requestKey, Parent: w.BaseSave}); err != nil {
		if errors.Is(err, store.ErrDuplicateKey) {
			return e.saveForKey(ctx, requestKey, id) // lost a race with the same key
		}
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
	defer e.kickUploads()
	if err := e.store.MarkSaveReady(ctx, saveID, m.Size()); err != nil {
		return nil, err
	}
	if err := e.store.SetBaseSave(ctx, id, saveID); err != nil {
		return nil, err
	}
	if kind == save.Full {
		if err := e.store.SetWorkspaceState(ctx, id, store.Stopped); err != nil {
			return nil, err
		}
	}
	return e.GetSave(ctx, saveID)
}

// checkSpace refuses a save that would leave less than MinFreeBytes free. The
// filesystem size is a lower bound; a full save also needs room for memory.
func (e *Engine) checkSpace(spec runtime.Spec) error {
	size, err := dirSize(spec.RootDir)
	if err != nil {
		return err
	}
	free, err := freeBytes(e.savesDir())
	if err != nil {
		return err
	}
	need := uint64(size) + uint64(spec.Limits.MemoryMB)<<20 + uint64(e.cfg.MinFreeBytes)
	if free < need {
		return fmt.Errorf("%w: save needs about %d MB plus %d MB headroom, %d MB free",
			ErrInsufficientStorage, size>>20, e.cfg.MinFreeBytes>>20, free>>20)
	}
	return nil
}

// needSpace refuses work that would write about size bytes to the engine's
// volume and leave less than MinFreeBytes free. Workspace disks are sparse,
// so their sizes do not reserve space: this check is what keeps a full volume
// from turning into I/O errors inside workspaces.
func (e *Engine) needSpace(what string, size int64) error {
	free, err := freeBytes(e.cfg.DataDir)
	if err != nil {
		return err
	}
	if need := uint64(size) + uint64(e.cfg.MinFreeBytes); free < need {
		return fmt.Errorf("%w: %s needs about %d MB plus %d MB headroom, %d MB free",
			ErrInsufficientStorage, what, size>>20, e.cfg.MinFreeBytes>>20, free>>20)
	}
	return nil
}

// noSpace reports a host volume running out during a write as insufficient
// storage (a workspace's own full disk is diskFull's business).
func noSpace(err error) error {
	if err != nil && errors.Is(err, syscall.ENOSPC) && !errors.Is(err, ErrInsufficientStorage) {
		return fmt.Errorf("%w: %v", ErrInsufficientStorage, err)
	}
	return err
}

func (e *Engine) writeSave(ctx context.Context, spec runtime.Spec, saveID string, kind save.Kind, now time.Time) (*save.Manifest, error) {
	var info *save.WorkspaceInfo
	if w, err := e.store.GetWorkspace(ctx, spec.ID); err == nil {
		info = &save.WorkspaceInfo{Name: w.Name, Spec: w.Spec}
	}
	b, err := save.NewBuilder(e.savesDir(), save.Manifest{
		SaveID:         saveID,
		WorkspaceID:    spec.ID,
		Kind:           kind,
		CreatedAt:      now,
		Runtime:        e.cfg.Runtime.Name(),
		RuntimeVersion: e.cfg.Runtime.Version(),
		Workspace:      info,
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

	// A full save checkpoints first: that stops the sandbox, so the filesystem is
	// quiet when it is archived and the two halves describe the same moment.
	if kind == save.Full {
		memDir, err := b.MemoryDir()
		if err != nil {
			return nil, err
		}
		if err := e.cfg.Runtime.Checkpoint(ctx, spec, memDir); err != nil {
			return nil, fmt.Errorf("save memory: %w", err)
		}
		if err := b.AddMemory(); err != nil {
			return nil, err
		}
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(save.TarDir(spec.RootDir, pw)) }()
	var src io.Reader = pr
	if err := e.cfg.Failpoints.Check(failpoint.SaveWrite); err != nil {
		src = &failingReader{r: pr, err: err}
	}
	if err := b.WriteFile(save.RootfsFile, src); err != nil {
		pr.CloseWithError(err)
		if errors.Is(err, syscall.ENOSPC) {
			err = fmt.Errorf("%w: %v", ErrInsufficientStorage, err)
		}
		return nil, fmt.Errorf("save filesystem: %w", err)
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
	return e.toSave(sv), nil
}

// DeleteSave hides a save: it can no longer be resumed or copied, but its files
// are kept for the retention window so it can be undeleted. Deleting an already
// deleted save does nothing.
func (e *Engine) DeleteSave(ctx context.Context, id string) error {
	sv, err := e.store.GetSave(ctx, id)
	if err != nil {
		return err
	}
	if sv.Auto {
		return errAutoSave(id)
	}
	switch sv.State {
	case store.SavePending:
		return ErrNotFound
	case store.SaveDeleted:
		return nil
	}
	return e.store.SoftDeleteSave(ctx, id, e.cfg.Now())
}

// UndeleteSave brings back a deleted save within its retention window.
// Undeleting a save that is not deleted does nothing.
func (e *Engine) UndeleteSave(ctx context.Context, id string) (*Save, error) {
	sv, err := e.store.GetSave(ctx, id)
	if err != nil {
		return nil, err
	}
	switch sv.State {
	case store.SavePending:
		return nil, ErrNotFound
	case store.SaveDeleted:
		if e.cfg.Now().After(sv.DeletedAt.Add(e.cfg.Retention)) {
			return nil, fmt.Errorf("%w: save %s is past its retention window and is being purged", ErrConflict, id)
		}
		state := store.SaveReady
		if _, err := os.Stat(filepath.Join(e.saveDir(id), save.ManifestFile)); err != nil {
			state = store.SaveMissing
		}
		if err := e.store.RestoreSave(ctx, id); err != nil {
			return nil, err
		}
		if state == store.SaveMissing {
			if err := e.store.SetSaveState(ctx, id, state); err != nil {
				return nil, err
			}
		}
	}
	return e.GetSave(ctx, id)
}

// VerifySave re-checks one save's files against its manifest and records the
// result: ready saves that fail become corrupt; a corrupt save that passes again
// (for example after its files were restored from a backup) becomes ready.
func (e *Engine) VerifySave(ctx context.Context, id string) (*Save, error) {
	sv, err := e.store.GetSave(ctx, id)
	if err != nil {
		return nil, err
	}
	switch sv.State {
	case store.SavePending:
		return nil, ErrNotFound
	case store.SaveReady, store.SaveCorrupt, store.SaveMissing:
		if e.remoteOn() && sv.Uploaded {
			// missing or damaged here: the copy in object storage stands in
			_, statErr := os.Stat(filepath.Join(e.saveDir(id), save.ManifestFile))
			_, verr := save.Verify(e.saveDir(id))
			if statErr != nil || verr != nil {
				if err := e.fetch(ctx, id); err != nil {
					e.log.Error("could not fetch save from object storage", "save", id, "err", err)
				}
			}
		}
		state := store.SaveReady
		if _, err := os.Stat(filepath.Join(e.saveDir(id), save.ManifestFile)); err != nil {
			state = store.SaveMissing
		} else if _, err := save.Verify(e.saveDir(id)); err != nil {
			if !errors.Is(err, save.ErrCorrupt) {
				return nil, err
			}
			e.log.Error("scrub: save failed verification", "save", id, "err", err)
			state = store.SaveCorrupt
		}
		if state != sv.State {
			if err := e.store.SetSaveState(ctx, id, state); err != nil {
				return nil, err
			}
		}
	}
	return e.GetSave(ctx, id)
}

// Scrub verifies every save that could be restored, so damage is found by the
// engine instead of by the next resume. It returns how many saves it checked
// and how many are not ready afterwards.
func (e *Engine) Scrub(ctx context.Context) (checked, bad int, err error) {
	all, err := e.store.AllSaves(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, sv := range all {
		if ctx.Err() != nil {
			return checked, bad, ctx.Err()
		}
		if sv.State != store.SaveReady && sv.State != store.SaveCorrupt && sv.State != store.SaveMissing {
			continue
		}
		got, err := e.VerifySave(ctx, sv.ID)
		if err != nil {
			return checked, bad, err
		}
		checked++
		if got.State != store.SaveReady {
			bad++
		}
	}
	return checked, bad, nil
}

// PurgeExpired permanently removes deleted saves whose retention window has
// ended. The folder goes first, then the record, so a crash in between leaves a
// record that the next purge finishes.
func (e *Engine) PurgeExpired(ctx context.Context) (int, error) {
	expired, err := e.store.DeletedBefore(ctx, e.cfg.Now().Add(-e.cfg.Retention))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sv := range expired {
		if err := e.deleteRemote(ctx, sv.ID); err != nil {
			return n, fmt.Errorf("purge save %s from object storage: %w", sv.ID, err)
		}
		if err := os.RemoveAll(e.saveDir(sv.ID)); err != nil {
			return n, err
		}
		if err := e.cfg.Failpoints.Check(failpoint.PurgeAfterRemove); err != nil {
			return n, err
		}
		if err := e.store.DeleteSave(ctx, sv.ID); err != nil {
			return n, err
		}
		n++
		e.log.Info("purged deleted save", "save", sv.ID, "deleted_at", sv.DeletedAt)
	}
	return n, nil
}

func (e *Engine) ListSaves(ctx context.Context, workspaceID string) ([]Save, error) {
	_, werr := e.store.GetWorkspace(ctx, workspaceID)
	if werr != nil && !errors.Is(werr, store.ErrNotFound) {
		return nil, werr
	}
	svs, err := e.store.ListSaves(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	// a workspace this server never had can still have saves here (made on
	// another server, learned from object storage): only no saves is not found
	if werr != nil && len(svs) == 0 {
		return nil, werr
	}
	out := make([]Save, 0, len(svs))
	for i := range svs {
		out = append(out, *e.toSave(&svs[i]))
	}
	return out, nil
}

// saveForKey returns the save a request key already made, nil if the key is
// unused, or an error if it was used for another workspace or is in progress.
func (e *Engine) saveForKey(ctx context.Context, key, workspaceID string) (*Save, error) {
	sv, err := e.store.SaveByRequestKey(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sv.WorkspaceID != workspaceID {
		return nil, fmt.Errorf("%w: idempotency key was already used for a different request", ErrConflict)
	}
	if sv.State == store.SavePending {
		return nil, fmt.Errorf("%w: a request with this idempotency key is still in progress", ErrConflict)
	}
	return e.toSave(sv), nil
}

// readySave loads a save and verifies every byte before anything uses it.
func (e *Engine) readySave(ctx context.Context, saveID string) (*store.Save, *save.Manifest, error) {
	sv, err := e.store.GetSave(ctx, saveID)
	if err != nil {
		return nil, nil, err
	}
	if sv.Auto {
		return nil, nil, errAutoSave(saveID)
	}
	switch sv.State {
	case store.SaveReady:
	case store.SavePending:
		return nil, nil, ErrNotFound
	case store.SaveCorrupt:
		return nil, nil, fmt.Errorf("%w: save %s failed verification earlier", ErrCorrupt, saveID)
	case store.SaveDeleted:
		return nil, nil, fmt.Errorf("%w: save %s is deleted; undelete it first", ErrConflict, saveID)
	default:
		return nil, nil, fmt.Errorf("%w: save %s is %s", ErrConflict, saveID, sv.State)
	}
	if err := e.ensureLocal(ctx, sv); err != nil {
		return nil, nil, err
	}
	m, err := save.Verify(e.saveDir(saveID))
	if errors.Is(err, save.ErrCorrupt) && e.remoteOn() && sv.Uploaded {
		// damaged here, intact in object storage: fetch it again
		e.log.Error("save failed verification; fetching it again from object storage", "save", saveID, "err", err)
		if ferr := e.fetch(ctx, saveID); ferr == nil {
			m, err = save.Verify(e.saveDir(saveID))
		}
	}
	if err != nil {
		if errors.Is(err, save.ErrCorrupt) {
			e.log.Error("save failed verification", "save", saveID, "err", err)
			_ = e.store.SetSaveState(ctx, saveID, store.SaveCorrupt)
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
	release, err := e.heavy(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	id := sv.WorkspaceID
	if err := e.wake(ctx, id); err != nil {
		return nil, err
	}
	e.touch(id)
	l := e.lock(id)
	if err := l.lockAlone(ctx, e.cfg.ExclusiveWait, "resume"); err != nil {
		return nil, err
	}
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
	ws, err := specOf(w)
	if err != nil {
		return nil, err
	}
	if err := e.restoreInto(ctx, spec, ws, saveID, m, failpoint.ResumeBeforeSwap); err != nil {
		if !failpoint.Crashed(err) {
			_ = e.store.SetWorkspaceState(ctx, id, store.Running)
		}
		return nil, err
	}
	if err := e.cfg.Failpoints.Check(failpoint.ResumeAfterSwap); err != nil {
		return nil, err
	}
	if err := e.store.SetBaseSave(ctx, id, saveID); err != nil {
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
func (e *Engine) restoreInto(ctx context.Context, spec runtime.Spec, ws Spec, saveID string, m *save.Manifest, beforeSwap string) error {
	root, next, prev := e.volume(spec.ID, "rootfs", ws), e.volume(spec.ID, "rootfs.next", ws), e.volume(spec.ID, "rootfs.prev", ws)
	if info, err := os.Stat(filepath.Join(e.saveDir(saveID), save.RootfsFile)); err == nil {
		if err := e.needSpace("restoring the save", info.Size()); err != nil {
			return err
		}
	}
	err := e.extractSave(saveID, next, ws)
	if errors.Is(err, ErrInsufficientStorage) && next.image {
		// A save of a full disk can need a few more blocks than it had once
		// extracted into a fresh filesystem, which allocates differently. Give
		// it that room rather than strand the save: the workspace comes back
		// exactly as full as it was.
		next.sizeMB += max(64, next.sizeMB/16)
		e.log.Info("save needs more room than its disk once extracted; growing the disk", "save", saveID, "disk_mb", next.sizeMB)
		err = e.extractSave(saveID, next, ws)
	}
	if err != nil {
		return noSpace(err)
	}
	if err := e.cfg.Failpoints.Check(beforeSwap); err != nil {
		return err
	}
	if root.exists() {
		if err := root.renameTo(prev); err != nil {
			return err
		}
	}
	if err := next.renameTo(root); err != nil {
		return err
	}
	if err := root.attach(); err != nil {
		return err
	}
	_ = prev.remove()

	if m.Kind == save.Full {
		return e.cfg.Runtime.Restore(ctx, spec, filepath.Join(e.saveDir(saveID), save.MemoryDir))
	}
	return e.cfg.Runtime.Start(ctx, spec)
}

// extractSave builds the save's filesystem in v, unmounted when done. On
// failure nothing of v is left.
func (e *Engine) extractSave(saveID string, v volume, ws Spec) error {
	if err := v.remove(); err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(e.saveDir(saveID), save.RootfsFile))
	if err != nil {
		return err
	}
	defer f.Close()
	var limit int64
	if info, statErr := f.Stat(); statErr == nil {
		limit = info.Size()
	}
	err = v.prepare()
	if err == nil {
		err = diskFull(save.UntarDir(f, v.path, limit), ws)
	}
	if err == nil {
		err = v.release()
	}
	if err != nil {
		_ = v.remove()
	}
	return err
}

// Copy makes count new workspaces from one save. Each copy has its own ID and
// identity; the original workspace is untouched.
//
// requestKey, if set, makes the request idempotent: copy i is created with key
// "<requestKey>#i", so a retry returns the copies already made and creates only
// the ones a crash prevented.
func (e *Engine) Copy(ctx context.Context, saveID string, count int, requestKey string) ([]Workspace, error) {
	if count < 1 || count > MaxCopies {
		return nil, fmt.Errorf("%w: count must be between 1 and %d", ErrInvalid, MaxCopies)
	}
	sv, m, err := e.readySave(ctx, saveID)
	if err != nil {
		return nil, err
	}
	release, err := e.heavy(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	// the original workspace's records, or (a save made on another server)
	// what its manifest says about it
	var srcName string
	var spec Spec
	if src, err := e.store.GetWorkspace(ctx, sv.WorkspaceID); err == nil {
		srcName = src.Name
		if spec, err = specOf(src); err != nil {
			return nil, err
		}
	} else if errors.Is(err, store.ErrNotFound) && m.Workspace != nil {
		srcName = m.Workspace.Name
		if err := json.Unmarshal(m.Workspace.Spec, &spec); err != nil {
			return nil, fmt.Errorf("%w: save %s describes its workspace unreadably: %v", ErrCorrupt, saveID, err)
		}
	} else if errors.Is(err, store.ErrNotFound) {
		srcName = sv.WorkspaceID // an older save from elsewhere: default settings
	} else {
		return nil, err
	}
	e.diskFor(&spec) // the copy keeps the original's size, in this engine's kind of disk

	out := make([]Workspace, 0, count)
	for i := 0; i < count; i++ {
		key := ""
		if requestKey != "" {
			key = fmt.Sprintf("%s#%d", requestKey, i)
			existing, err := e.workspaceForKey(ctx, key, saveID)
			if err != nil {
				return out, err
			}
			if existing != nil {
				out = append(out, *existing)
				continue
			}
		}
		id := newID("ws")
		w, err := e.insertWorkspace(ctx, id, fmt.Sprintf("%s-copy-%d", srcName, i+1), spec, saveID, key)
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
		if err := e.restoreInto(ctx, rs, spec, saveID, m, failpoint.CopyBeforeRunning); err != nil {
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
		if paused, err := e.recoverPause(ctx, w); err != nil {
			return err
		} else if paused {
			continue
		}
		if w.State == store.Creating && w.SourceSave != "" {
			// a copy interrupted before it was handed out: the caller never got it,
			// and a retry with the same idempotency key must be able to make it again
			if err := e.store.SetWorkspaceState(ctx, w.ID, store.Deleted); err != nil {
				return err
			}
			if err := e.store.ReleaseWorkspaceRequestKey(ctx, w.ID); err != nil {
				return err
			}
			if spec, err := specOf(w); err == nil {
				_ = e.removeWorkspaceDir(w.ID, spec)
			}
			e.log.Info("discarded interrupted copy", "workspace", w.ID)
			continue
		}
		spec, err := specOf(w)
		if err != nil {
			return err
		}
		if err := e.finishSwap(w.ID, spec); err != nil {
			return err
		}
		// a template copy cut off mid-extraction
		if err := e.volume(w.ID, "rootfs.new", spec).remove(); err != nil {
			return err
		}
		if err := e.createRootfs(w.ID, spec); err != nil {
			e.log.Error("could not rebuild workspace filesystem", "workspace", w.ID, "err", err)
			empty := spec
			empty.Template = "" // keep the workspace usable, with an empty filesystem of its kind
			if err := e.createRootfs(w.ID, empty); err != nil {
				return err
			}
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
		// every workspace's files must be in place, stopped and paused ones
		// included: they can be read, and the host may have rebooted since
		ws, err := specOf(&live[i])
		if err != nil {
			return err
		}
		if err := e.volume(live[i].ID, "rootfs", ws).attach(); err != nil {
			e.log.Error("could not mount workspace disk", "workspace", live[i].ID, "err", err)
			continue
		}
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
func (e *Engine) finishSwap(id string, spec Spec) error {
	root, next, prev := e.volume(id, "rootfs", spec), e.volume(id, "rootfs.next", spec), e.volume(id, "rootfs.prev", spec)
	if !root.exists() {
		// crashed between the two renames: the new filesystem is complete, finish
		if next.exists() {
			if err := next.renameTo(root); err != nil {
				return err
			}
		} else if prev.exists() {
			if err := prev.renameTo(root); err != nil {
				return err
			}
		}
	}
	if err := next.remove(); err != nil { // never swapped in: the old filesystem stays
		return err
	}
	return prev.remove()
}

// --- helpers ---

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

func (e *Engine) workspaceDir(id string) string {
	return filepath.Join(e.cfg.DataDir, "workspaces", id)
}
func (e *Engine) rootDir(id string) string { return filepath.Join(e.workspaceDir(id), "rootfs") }
func (e *Engine) savesDir() string         { return filepath.Join(e.cfg.DataDir, "saves") }
func (e *Engine) saveDir(id string) string { return filepath.Join(e.savesDir(), id) }

// SaveDir is where a save lives on disk; tests use it to inject corruption.
func (e *Engine) SaveDir(id string) string { return e.saveDir(id) }

// specOf decodes what a workspace was created with.
func specOf(w *store.Workspace) (Spec, error) {
	var spec Spec
	if len(w.Spec) > 0 {
		if err := json.Unmarshal(w.Spec, &spec); err != nil {
			return Spec{}, err
		}
	}
	return spec, nil
}

func toWorkspace(w *store.Workspace) (*Workspace, error) {
	var spec Spec
	if len(w.Spec) > 0 {
		if err := json.Unmarshal(w.Spec, &spec); err != nil {
			return nil, err
		}
	}
	return &Workspace{
		ID: w.ID, Name: w.Name, State: w.State, Limits: spec.Limits, Network: spec.Network,
		Template: spec.Template, SourceSave: w.SourceSave, BaseSave: w.BaseSave, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}, nil
}

func (e *Engine) toSave(sv *store.Save) *Save {
	out := &Save{ID: sv.ID, WorkspaceID: sv.WorkspaceID, Kind: save.Kind(sv.Kind), State: sv.State, Size: sv.Size, CreatedAt: sv.CreatedAt, Auto: sv.Auto, Uploaded: sv.Uploaded, Parent: sv.Parent}
	if !sv.DeletedAt.IsZero() {
		d, p := sv.DeletedAt, sv.DeletedAt.Add(e.cfg.Retention)
		out.DeletedAt, out.PurgeAfter = &d, &p
	}
	return out
}

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
