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
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite, so cairnd stays one static binary
)

// ErrNotFound is returned when a workspace or save does not exist.
var ErrNotFound = errors.New("not found")

// ErrDuplicateKey is returned when an object with the same request key exists.
var ErrDuplicateKey = errors.New("request key already used")

// WorkspaceState is where a workspace is in its lifecycle.
type WorkspaceState string

const (
	Creating WorkspaceState = "creating"
	Running  WorkspaceState = "running"
	Saving   WorkspaceState = "saving"   // a save is being written
	Stopped  WorkspaceState = "stopped"  // full-saved; holds no server resources
	Resuming WorkspaceState = "resuming" // filesystem or memory is being restored
	Pausing  WorkspaceState = "pausing"  // being saved to pause it
	Paused   WorkspaceState = "paused"   // saved and stopped by the engine; the next use wakes it
	Deleted  WorkspaceState = "deleted"
)

// Transitional states are ones the engine can crash in the middle of.
func (s WorkspaceState) Transitional() bool {
	return s == Creating || s == Saving || s == Resuming || s == Pausing
}

type Workspace struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	State      WorkspaceState  `json:"state"`
	Spec       json.RawMessage `json:"spec"`                  // limits, network, template
	SourceSave string          `json:"source_save,omitempty"` // set for copies
	PausedSave string          `json:"-"`                     // the save a paused workspace wakes from
	RequestKey string          `json:"-"`                     // caller's idempotency key, if any
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type SaveState string

const (
	SavePending SaveState = "pending" // being written; never restored from
	SaveReady   SaveState = "ready"
	SaveMissing SaveState = "missing" // recorded, but its folder is gone or fails verification
	SaveDeleted SaveState = "deleted" // hidden; kept until its retention window ends, then purged
	SaveCorrupt SaveState = "corrupt" // failed verification (scrub or restore); never restored from
)

type Save struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Kind        string    `json:"kind"`
	State       SaveState `json:"state"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	DeletedAt   time.Time `json:"deleted_at"` // zero unless deleted
	RequestKey  string    `json:"-"`          // caller's idempotency key, if any
	Auto        bool      `json:"auto"`       // made by the engine (pausing), not by a caller
}

type Store struct{ db *sql.DB }

// migrations bring a state database up to the current schema. Each runs once,
// in order, inside a transaction, and is recorded in PRAGMA user_version. Never
// edit a migration that has shipped: add a new one. A database newer than this
// engine is refused rather than misread.
var migrations = []string{
	// 1: initial schema
	`
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
`,
	// 2: soft delete and retention for saves
	`
ALTER TABLE saves ADD COLUMN deleted_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS saves_deleted ON saves (state, deleted_at);
`,
	// 3: idempotency keys, stored in the same row as the object they created,
	// so a retried request finds its result even after a crash
	`
ALTER TABLE workspaces ADD COLUMN request_key TEXT;
ALTER TABLE saves ADD COLUMN request_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_request_key ON workspaces (request_key) WHERE request_key IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS saves_request_key ON saves (request_key) WHERE request_key IS NOT NULL;
`,
	// 4: pausing: the engine saves idle workspaces (and all of them on shutdown)
	// and wakes them on next use
	`
ALTER TABLE workspaces ADD COLUMN paused_save TEXT NOT NULL DEFAULT '';
ALTER TABLE saves ADD COLUMN auto INTEGER NOT NULL DEFAULT 0;
`,
}

const (
	workspaceCols = `id, name, state, spec, source_save, paused_save, created_at, updated_at`
	saveCols      = `id, workspace_id, kind, state, size, created_at, deleted_at, auto`
)

// SchemaVersion is the schema this engine writes.
var SchemaVersion = len(migrations)

// Open opens (or creates) the state database at path.
func Open(path string) (*Store, error) {
	// WAL + synchronous=FULL: a committed state change survives a power cut.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialize here instead of retrying
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("state database is schema version %d, newer than this engine (%d): upgrade the engine", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate state database to version %d: %w", i+1, err)
		}
		// PRAGMA cannot take a bound parameter; i+1 is our own integer.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Version returns the database's schema version.
func (s *Store) Version() (int, error) {
	var v int
	err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateWorkspace(ctx context.Context, w Workspace) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, state, spec, source_save, created_at, updated_at, request_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Name, w.State, string(w.Spec), w.SourceSave, w.CreatedAt.UnixNano(), w.UpdatedAt.UnixNano(), nullable(w.RequestKey))
	return duplicate(err)
}

// WorkspaceByRequestKey finds the workspace a request key created.
func (s *Store) WorkspaceByRequestKey(ctx context.Context, key string) (*Workspace, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE request_key = ?`, key)
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// SaveByRequestKey finds the save a request key created.
func (s *Store) SaveByRequestKey(ctx context.Context, key string) (*Save, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+saveCols+` FROM saves WHERE request_key = ?`, key)
	sv, err := scanSave(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sv, err
}

// ReleaseWorkspaceRequestKey frees the key of a workspace that was discarded
// (a copy interrupted by a crash), so a retry can create it again.
func (s *Store) ReleaseWorkspaceRequestKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workspaces SET request_key = NULL WHERE id = ?`, id)
	return err
}

// Counts is how many workspaces and saves are in each state, and the bytes held
// by saves that are not purged.
type Counts struct {
	Workspaces map[WorkspaceState]int
	Saves      map[SaveState]int
	SaveBytes  int64
}

func (s *Store) Counts(ctx context.Context) (*Counts, error) {
	c := &Counts{Workspaces: map[WorkspaceState]int{}, Saves: map[SaveState]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT state, count(*) FROM workspaces GROUP BY state`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st WorkspaceState
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return nil, err
		}
		c.Workspaces[st] = n
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT state, count(*), coalesce(sum(size), 0) FROM saves GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st SaveState
		var n int
		var bytes int64
		if err := rows.Scan(&st, &n, &bytes); err != nil {
			return nil, err
		}
		c.Saves[st] = n
		c.SaveBytes += bytes
	}
	return c, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func duplicate(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: %v", ErrDuplicateKey, err)
	}
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

// SetPaused records that a workspace is paused and which save wakes it; an
// empty save clears it (the workspace is running again).
func (s *Store) SetPaused(ctx context.Context, id string, state WorkspaceState, pausedSave string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE workspaces SET state = ?, paused_save = ?, updated_at = ? WHERE id = ?`,
		state, pausedSave, time.Now().UnixNano(), id)
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
		`SELECT `+workspaceCols+` FROM workspaces WHERE id = ?`, id)
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// ListWorkspaces returns workspaces that are not deleted, oldest first.
func (s *Store) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	return s.queryWorkspaces(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE state != ? ORDER BY created_at, id`,
		Deleted)
}

// TransitionalWorkspaces are the ones a crash may have interrupted.
func (s *Store) TransitionalWorkspaces(ctx context.Context) ([]Workspace, error) {
	return s.queryWorkspaces(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE state IN (?, ?, ?, ?)`,
		Creating, Saving, Resuming, Pausing)
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
		`INSERT INTO saves (id, workspace_id, kind, state, size, created_at, request_key, auto) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sv.ID, sv.WorkspaceID, sv.Kind, sv.State, sv.Size, sv.CreatedAt.UnixNano(), nullable(sv.RequestKey), sv.Auto)
	return duplicate(err)
}

func (s *Store) GetSave(ctx context.Context, id string) (*Save, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+saveCols+` FROM saves WHERE id = ?`, id)
	sv, err := scanSave(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sv, err
}

// ListSaves returns a workspace's published, not deleted saves, newest first.
func (s *Store) ListSaves(ctx context.Context, workspaceID string) ([]Save, error) {
	return s.querySaves(ctx,
		`SELECT `+saveCols+` FROM saves WHERE workspace_id = ? AND state NOT IN (?, ?) ORDER BY created_at DESC, id DESC`,
		workspaceID, SavePending, SaveDeleted)
}

func (s *Store) AllSaves(ctx context.Context) ([]Save, error) {
	return s.querySaves(ctx, `SELECT `+saveCols+` FROM saves`)
}

// DeletedBefore returns deleted saves whose deletion is older than t.
func (s *Store) DeletedBefore(ctx context.Context, t time.Time) ([]Save, error) {
	return s.querySaves(ctx,
		`SELECT `+saveCols+` FROM saves WHERE state = ? AND deleted_at <= ?`,
		SaveDeleted, t.UnixNano())
}

// SoftDeleteSave marks a save deleted at t. Its files stay until purged.
func (s *Store) SoftDeleteSave(ctx context.Context, id string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE saves SET state = ?, deleted_at = ? WHERE id = ?`, SaveDeleted, t.UnixNano(), id)
	return err
}

// RestoreSave brings a deleted save back to ready.
func (s *Store) RestoreSave(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE saves SET state = ?, deleted_at = 0 WHERE id = ?`, SaveReady, id)
	return err
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
	if err := r.Scan(&w.ID, &w.Name, &w.State, &spec, &w.SourceSave, &w.PausedSave, &created, &updated); err != nil {
		return nil, err
	}
	w.Spec = json.RawMessage(spec)
	w.CreatedAt = time.Unix(0, created).UTC()
	w.UpdatedAt = time.Unix(0, updated).UTC()
	return &w, nil
}

func scanSave(r scanner) (*Save, error) {
	var sv Save
	var created, deleted int64
	if err := r.Scan(&sv.ID, &sv.WorkspaceID, &sv.Kind, &sv.State, &sv.Size, &created, &deleted, &sv.Auto); err != nil {
		return nil, err
	}
	sv.CreatedAt = time.Unix(0, created).UTC()
	if deleted != 0 {
		sv.DeletedAt = time.Unix(0, deleted).UTC()
	}
	return &sv, nil
}
