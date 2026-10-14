// Package store keeps the engine's state: which workspaces and saves exist and
// what state each is in. It is the engine's memory across restarts and crashes,
// so every state change is written here before the engine acts on it.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite, so cairnd stays one static binary
)

// ErrNotFound is returned when a workspace or save does not exist.
var ErrNotFound = errors.New("not found")

// WorkspaceState is where a workspace is in its lifecycle.
type WorkspaceState string

const (
	Creating WorkspaceState = "creating"
	Running  WorkspaceState = "running"
	Saving   WorkspaceState = "saving"   // a save is being written
	Stopped  WorkspaceState = "stopped"  // full-saved; holds no server resources
	Resuming WorkspaceState = "resuming" // filesystem or memory is being restored
	Deleted  WorkspaceState = "deleted"
)

// Transitional states are ones the engine can crash in the middle of.
func (s WorkspaceState) Transitional() bool {
	return s == Creating || s == Saving || s == Resuming
}

type Workspace struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	State      WorkspaceState  `json:"state"`
	Spec       json.RawMessage `json:"spec"`              // limits, network, template
	SourceSave string          `json:"source_save,omitempty"` // set for copies
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type SaveState string

const (
	SavePending SaveState = "pending" // being written; never restored from
	SaveReady   SaveState = "ready"
	SaveMissing SaveState = "missing" // recorded, but its folder is gone or fails verification
)

type Save struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Kind        string    `json:"kind"`
	State       SaveState `json:"state"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
}

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS workspaces (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	state       TEXT NOT NULL,
	spec        TEXT NOT NULL,
	source_save TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS saves (
	id           TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL,
	kind         TEXT NOT NULL,
	state        TEXT NOT NULL,
	size         INTEGER NOT NULL,
	created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS saves_by_workspace ON saves (workspace_id, created_at);
`

// Open opens (or creates) the state database at path.
func Open(path string) (*Store, error) {
	// WAL + synchronous=FULL: a committed state change survives a power cut.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialize here instead of retrying
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateWorkspace(ctx context.Context, w Workspace) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, state, spec, source_save, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Name, w.State, string(w.Spec), w.SourceSave, w.CreatedAt.UnixNano(), w.UpdatedAt.UnixNano())
	return err
}

func (s *Store) SetWorkspaceState(ctx context.Context, id string, state WorkspaceState) error {
	res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET state = ?, updated_at = ? WHERE id = ?`,
		state, time.Now().UnixNano(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, state, spec, source_save, created_at, updated_at FROM workspaces WHERE id = ?`, id)
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// ListWorkspaces returns workspaces that are not deleted, oldest first.
func (s *Store) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	return s.queryWorkspaces(ctx,
		`SELECT id, name, state, spec, source_save, created_at, updated_at FROM workspaces WHERE state != ? ORDER BY created_at, id`,
		Deleted)
}

// TransitionalWorkspaces are the ones a crash may have interrupted.
func (s *Store) TransitionalWorkspaces(ctx context.Context) ([]Workspace, error) {
	return s.queryWorkspaces(ctx,
		`SELECT id, name, state, spec, source_save, created_at, updated_at FROM workspaces WHERE state IN (?, ?, ?)`,
		Creating, Saving, Resuming)
}

func (s *Store) queryWorkspaces(ctx context.Context, q string, args ...any) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

func (s *Store) CreateSave(ctx context.Context, sv Save) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO saves (id, workspace_id, kind, state, size, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sv.ID, sv.WorkspaceID, sv.Kind, sv.State, sv.Size, sv.CreatedAt.UnixNano())
	return err
}

func (s *Store) GetSave(ctx context.Context, id string) (*Save, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, kind, state, size, created_at FROM saves WHERE id = ?`, id)
	sv, err := scanSave(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sv, err
}

// ListSaves returns a workspace's published saves, newest first.
func (s *Store) ListSaves(ctx context.Context, workspaceID string) ([]Save, error) {
	return s.querySaves(ctx,
		`SELECT id, workspace_id, kind, state, size, created_at FROM saves WHERE workspace_id = ? AND state != ? ORDER BY created_at DESC, id DESC`,
		workspaceID, SavePending)
}

func (s *Store) AllSaves(ctx context.Context) ([]Save, error) {
	return s.querySaves(ctx, `SELECT id, workspace_id, kind, state, size, created_at FROM saves`)
}

func (s *Store) SetSaveState(ctx context.Context, id string, state SaveState) error {
	_, err := s.db.ExecContext(ctx, `UPDATE saves SET state = ? WHERE id = ?`, state, id)
	return err
}

// MarkSaveReady publishes a save once its folder is committed.
func (s *Store) MarkSaveReady(ctx context.Context, id string, size int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE saves SET state = ?, size = ? WHERE id = ?`, SaveReady, size, id)
	return err
}

func (s *Store) DeleteSave(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM saves WHERE id = ?`, id)
	return err
}

func (s *Store) querySaves(ctx context.Context, q string, args ...any) ([]Save, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Save
	for rows.Next() {
		sv, err := scanSave(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sv)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanWorkspace(r scanner) (*Workspace, error) {
	var w Workspace
	var spec string
	var created, updated int64
	if err := r.Scan(&w.ID, &w.Name, &w.State, &spec, &w.SourceSave, &created, &updated); err != nil {
		return nil, err
	}
	w.Spec = json.RawMessage(spec)
	w.CreatedAt = time.Unix(0, created).UTC()
	w.UpdatedAt = time.Unix(0, updated).UTC()
	return &w, nil
}

func scanSave(r scanner) (*Save, error) {
	var sv Save
	var created int64
	if err := r.Scan(&sv.ID, &sv.WorkspaceID, &sv.Kind, &sv.State, &sv.Size, &created); err != nil {
		return nil, err
	}
	sv.CreatedAt = time.Unix(0, created).UTC()
	return &sv, nil
}
