package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/internal/save"
	"github.com/AQLD/cairn/internal/store"
)

// The state database records every workspace and save. It is not the only
// copy of what matters: saves carry their own manifests, and each workspace's
// folder holds a record of how it was created (workspace.json). So a database
// lost or damaged on disk is rebuilt, not mourned: from its last backup
// (state.db.backup, written regularly with BackupState) plus what the data
// folder holds now (Config.RestoreState, cairnd --restore-state).

func (e *Engine) statePath() string  { return filepath.Join(e.cfg.DataDir, "state.db") }
func (e *Engine) backupPath() string { return e.statePath() + ".backup" }

// recordFile is a workspace's own record of its creation.
func (e *Engine) recordFile(id string) string {
	return filepath.Join(e.workspaceDir(id), "workspace.json")
}

type workspaceRecord struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Spec       json.RawMessage `json:"spec"`
	SourceSave string          `json:"source_save,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// writeRecord writes a new workspace's record into its folder, durably.
func (e *Engine) writeRecord(w *store.Workspace) error {
	if err := os.MkdirAll(e.workspaceDir(w.ID), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(workspaceRecord{ID: w.ID, Name: w.Name, Spec: w.Spec, SourceSave: w.SourceSave, CreatedAt: w.CreatedAt})
	if err != nil {
		return err
	}
	p := e.recordFile(w.ID)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	if err := syncFile(p); err != nil {
		return err
	}
	return syncParents(p)
}

// BackupState writes a consistent copy of the state database next to it.
// cairnd does this at startup and then regularly.
func (e *Engine) BackupState(ctx context.Context) error {
	return e.store.Backup(ctx, e.backupPath())
}

// damagedState explains a database that failed its check and how to recover.
func (e *Engine) damagedState(err error) error {
	backup := "no backup exists yet, so it will be rebuilt from the data folder alone"
	if _, serr := os.Stat(e.backupPath()); serr == nil {
		backup = "from its last backup (" + e.backupPath() + ") and the data folder"
	}
	return fmt.Errorf("%w; nothing was changed. Start the engine once with --restore-state to rebuild it %s", err, backup)
}

// stateNeedsRestore reports whether the state database is missing (while the
// data folder holds workspaces or saves) or fails its check.
func (e *Engine) stateNeedsRestore() (bool, error) {
	if _, err := os.Stat(e.statePath()); errors.Is(err, os.ErrNotExist) {
		ws, _ := os.ReadDir(filepath.Join(e.cfg.DataDir, "workspaces"))
		sv, _ := os.ReadDir(e.savesDir())
		return len(ws)+len(sv) > 0, nil
	}
	st, err := store.Open(e.statePath())
	if errors.Is(err, store.ErrDamaged) {
		e.log.Warn("state database is damaged; rebuilding it", "err", err)
		return true, nil
	}
	if err != nil {
		return false, err
	}
	e.log.Info("state database is healthy; nothing to restore")
	return false, st.Close()
}

// restoreStateFiles moves the current database aside and puts the last backup
// (if any) in its place.
func (e *Engine) restoreStateFiles() error {
	stamp := e.cfg.Now().UTC().Format("20060102T150405")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := e.statePath() + suffix
		if _, err := os.Stat(p); err == nil {
			if err := os.Rename(p, e.statePath()+".damaged-"+stamp+suffix); err != nil {
				return err
			}
		}
	}
	src, err := os.Open(e.backupPath())
	if errors.Is(err, os.ErrNotExist) {
		e.log.Warn("no state backup: rebuilding the state database from the data folder alone")
		return nil
	}
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(e.statePath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if err == nil {
		err = fsync(dst)
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = syncDir(e.cfg.DataDir)
	}
	return err
}

// reconcile brings a restored database in line with the data folder: what is
// on disk but not recorded is added back, what is recorded but gone is
// dropped. Saves are only added back if they verify.
func (e *Engine) reconcile(ctx context.Context) error {
	// saves
	entries, err := os.ReadDir(e.savesDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	onDisk := map[string]bool{}
	for _, d := range entries {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		onDisk[d.Name()] = true
		if _, err := e.store.GetSave(ctx, d.Name()); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		m, err := save.Verify(e.saveDir(d.Name()))
		if err != nil {
			e.log.Error("save on disk does not verify; not restored", "save", d.Name(), "err", err)
			continue
		}
		sv := store.Save{ID: m.SaveID, WorkspaceID: m.WorkspaceID, Kind: string(m.Kind), State: store.SaveReady, Size: m.Size(), CreatedAt: m.CreatedAt}
		if err := e.store.CreateSave(ctx, sv); err != nil {
			return err
		}
		e.log.Info("restored save from disk", "save", sv.ID, "workspace", sv.WorkspaceID)
	}
	saves, err := e.store.AllSaves(ctx)
	if err != nil {
		return err
	}
	for _, sv := range saves {
		if !onDisk[sv.ID] { // removed (or purged) after the backup
			if err := e.store.DeleteSave(ctx, sv.ID); err != nil {
				return err
			}
		}
	}

	// workspaces
	dirs, err := os.ReadDir(filepath.Join(e.cfg.DataDir, "workspaces"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	present := map[string]bool{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		present[d.Name()] = true
		if _, err := e.store.GetWorkspace(ctx, d.Name()); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		b, err := os.ReadFile(e.recordFile(d.Name()))
		if err != nil {
			e.log.Error("workspace folder has no record (made before records existed); not restored", "workspace", d.Name())
			continue
		}
		var r workspaceRecord
		if err := json.Unmarshal(b, &r); err != nil {
			e.log.Error("workspace record unreadable; not restored", "workspace", d.Name(), "err", err)
			continue
		}
		now := e.cfg.Now().UTC()
		w := store.Workspace{ID: r.ID, Name: r.Name, State: store.Running, Spec: r.Spec, SourceSave: r.SourceSave, CreatedAt: r.CreatedAt, UpdatedAt: now}
		if err := e.store.CreateWorkspace(ctx, w); err != nil {
			return err
		}
		e.log.Info("restored workspace from disk", "workspace", w.ID)
	}
	live, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	for _, w := range live {
		switch {
		case !present[w.ID]: // deleted after the backup
			if err := e.store.SetWorkspaceState(ctx, w.ID, store.Deleted); err != nil {
				return err
			}
		case w.State == store.Paused && !onDisk[w.PausedSave]: // woken (and its pause save dropped) after the backup
			if err := e.store.SetPaused(ctx, w.ID, store.Running, ""); err != nil {
				return err
			}
		}
	}
	return nil
}
