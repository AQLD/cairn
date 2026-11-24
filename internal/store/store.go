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
	"os"
	"path/filepath"
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
	// Warm workspaces are created and started ahead of demand and belong to
	// no one until a create claims one (it becomes running); warming ones are
	// still being built. Neither is listed or reachable by ID.
	Warm    WorkspaceState = "warm"
	Warming WorkspaceState = "warming"
)

// Hidden states are ones callers never see: deleted, or not yet anyone's.
func (s WorkspaceState) Hidden() bool { return s == Deleted || s == Warm || s == Warming }

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
	BaseSave   string          `json:"base_save,omitempty"`   // the save its state comes from: last saved, resumed or copied
	RequestKey string          `json:"-"`                     // caller's idempotency key, if any
	Owner      string          `json:"owner,omitempty"`       // the API key that made it, if any
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
	Uploaded    bool      `json:"uploaded"`   // a complete copy is in object storage
	Parent      string    `json:"parent"`     // the save the workspace's state came from when this one was made
	Pinned      bool      `json:"pinned"`     // kept by pruning and refused by delete until unpinned
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
	// 5: saves copied to object storage
	`
ALTER TABLE saves ADD COLUMN uploaded INTEGER NOT NULL DEFAULT 0;
`,
	// 6: history: each save records the save its workspace's state came from,
	// so a workspace's saves form a tree (rewinding and saving again branches)
	`
ALTER TABLE workspaces ADD COLUMN base_save TEXT NOT NULL DEFAULT '';
ALTER TABLE saves ADD COLUMN parent TEXT NOT NULL DEFAULT '';
UPDATE workspaces SET base_save = source_save;
`,
	// 7: pinned saves, which pruning keeps and delete refuses
	`
ALTER TABLE saves ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
`,
	// 8: secrets, values sealed with a key kept outside the data folder
	`
CREATE TABLE secrets (
	name       TEXT PRIMARY KEY,
	hosts      TEXT NOT NULL,
	sealed     BLOB NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
`,
	// 9: API keys, each caller's own; only the token's SHA-256 is kept
	`
CREATE TABLE api_keys (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	scope      TEXT NOT NULL,
	hash       TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	revoked_at INTEGER NOT NULL DEFAULT 0
);
`,
	// 10: who made each workspace (an API key's ID), and keys that may use
	// only their own workspaces
	`
ALTER TABLE workspaces ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN own INTEGER NOT NULL DEFAULT 0;
`,
}

const (
	workspaceCols = `id, name, state, spec, source_save, paused_save, base_save, created_at, updated_at, owner`
	saveCols      = `id, workspace_id, kind, state, size, created_at, deleted_at, auto, uploaded, parent, pinned`
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
	s := &Store{db: db}
	if err := s.Check(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db, path); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ErrDamaged: the state database fails its integrity check.
var ErrDamaged = errors.New("state database is damaged")

// Check runs SQLite's integrity check (quick_check: structure, not every index).
func (s *Store) Check(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("%w: %v", ErrDamaged, err)
		}
		if line != "ok" && len(problems) < 5 {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrDamaged, strings.Join(problems, "; "))
	}
	return nil
}

// Backup writes a consistent copy of the database to path, durably, while the
// database stays in use: a crash during it leaves the previous backup intact.
func (s *Store) Backup(ctx context.Context, path string) error {
	return backup(ctx, s.db, path)
}

func backup(ctx context.Context, db *sql.DB, path string) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	// VACUUM INTO takes no bound parameter; the path is the engine's own.
	if _, err := db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(tmp, "'", "''")+`'`); err != nil {
		return fmt.Errorf("back up state database: %w", err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func migrate(db *sql.DB, path string) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("state database is schema version %d, newer than this engine (%d): upgrade the engine", v, len(migrations))
	}
	if v > 0 && v < len(migrations) {
		// an upgrade changes the schema: keep the database as it was first
		if err := backup(context.Background(), db, fmt.Sprintf("%s.before-v%d", path, len(migrations))); err != nil {
			return err
		}
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
		`INSERT INTO workspaces (id, name, state, spec, source_save, base_save, created_at, updated_at, request_key, owner) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Name, w.State, string(w.Spec), w.SourceSave, w.BaseSave, w.CreatedAt.UnixNano(), w.UpdatedAt.UnixNano(), nullable(w.RequestKey), w.Owner)
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

// ListWorkspaces returns callers' workspaces (not deleted, not warm), oldest first.
func (s *Store) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	return s.queryWorkspaces(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE state NOT IN (?, ?, ?) ORDER BY created_at, id`,
		Deleted, Warm, Warming)
}

// WarmWorkspaces returns the warm and warming workspaces.
func (s *Store) WarmWorkspaces(ctx context.Context) ([]Workspace, error) {
	return s.queryWorkspaces(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE state IN (?, ?) ORDER BY created_at, id`,
		Warm, Warming)
}

// ClaimWarm hands a warm workspace made with exactly spec to a caller: it
// becomes running under name (its own ID if name is empty) and requestKey,
// created now. Atomic, so two creates never get the same one. ErrNotFound if
// none is warm.
func (s *Store) ClaimWarm(ctx context.Context, spec json.RawMessage, name, requestKey, owner string, now time.Time) (*Workspace, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE workspaces SET state = ?, name = CASE WHEN ? = '' THEN id ELSE ? END, request_key = ?, owner = ?, created_at = ?, updated_at = ?
		 WHERE id = (SELECT id FROM workspaces WHERE state = ? AND spec = ? ORDER BY created_at, id LIMIT 1)
		 RETURNING `+workspaceCols,
		Running, name, name, nullable(requestKey), owner, now.UnixNano(), now.UnixNano(), Warm, string(spec))
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, duplicate(err)
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
		`INSERT INTO saves (id, workspace_id, kind, state, size, created_at, request_key, auto, uploaded, parent) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sv.ID, sv.WorkspaceID, sv.Kind, sv.State, sv.Size, sv.CreatedAt.UnixNano(), nullable(sv.RequestKey), sv.Auto, sv.Uploaded, sv.Parent)
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

// SetPinned pins or unpins a save.
func (s *Store) SetPinned(ctx context.Context, id string, pinned bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE saves SET pinned = ? WHERE id = ?`, pinned, id)
	return err
}

// RestoreSave brings a deleted save back to ready.
func (s *Store) RestoreSave(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE saves SET state = ?, deleted_at = 0 WHERE id = ?`, SaveReady, id)
	return err
}

// SetBaseSave records the save a workspace's state now comes from.
func (s *Store) SetBaseSave(ctx context.Context, workspaceID, saveID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE workspaces SET base_save = ? WHERE id = ?`, saveID, workspaceID)
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

// MarkUploaded records that a complete copy of the save is in object storage.
func (s *Store) MarkUploaded(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE saves SET uploaded = 1 WHERE id = ?`, id)
	return err
}

// PendingUploads lists ready saves not yet in object storage, oldest first.
func (s *Store) PendingUploads(ctx context.Context) ([]Save, error) {
	return s.querySaves(ctx, `SELECT `+saveCols+` FROM saves WHERE state = ? AND uploaded = 0 ORDER BY created_at`, SaveReady)
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
	if err := r.Scan(&w.ID, &w.Name, &w.State, &spec, &w.SourceSave, &w.PausedSave, &w.BaseSave, &created, &updated, &w.Owner); err != nil {
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
	if err := r.Scan(&sv.ID, &sv.WorkspaceID, &sv.Kind, &sv.State, &sv.Size, &created, &deleted, &sv.Auto, &sv.Uploaded, &sv.Parent, &sv.Pinned); err != nil {
		return nil, err
	}
	sv.CreatedAt = time.Unix(0, created).UTC()
	if deleted != 0 {
		sv.DeletedAt = time.Unix(0, deleted).UTC()
	}
	return &sv, nil
}

// Secret is a stored secret; Sealed is its value encrypted (internal/secrets).
type Secret struct {
	Name      string
	Hosts     []string
	Sealed    []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PutSecret creates or replaces a secret.
func (s *Store) PutSecret(ctx context.Context, sec Secret) error {
	hosts, err := json.Marshal(sec.Hosts)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO secrets (name, hosts, sealed, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET hosts = excluded.hosts, sealed = excluded.sealed, updated_at = excluded.updated_at`,
		sec.Name, string(hosts), sec.Sealed, sec.CreatedAt.UnixNano(), sec.UpdatedAt.UnixNano())
	return err
}

// GetSecret returns a secret, or ErrNotFound.
func (s *Store) GetSecret(ctx context.Context, name string) (Secret, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, hosts, sealed, created_at, updated_at FROM secrets WHERE name = ?`, name)
	if err != nil {
		return Secret{}, err
	}
	list, err := scanSecrets(rows)
	if err != nil {
		return Secret{}, err
	}
	if len(list) == 0 {
		return Secret{}, ErrNotFound
	}
	return list[0], nil
}

// ListSecrets returns every secret, by name.
func (s *Store) ListSecrets(ctx context.Context) ([]Secret, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, hosts, sealed, created_at, updated_at FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return scanSecrets(rows)
}

// DeleteSecret removes a secret; ErrNotFound if there is none.
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanSecrets(rows *sql.Rows) ([]Secret, error) {
	defer rows.Close()
	var out []Secret
	for rows.Next() {
		var sec Secret
		var hosts string
		var created, updated int64
		if err := rows.Scan(&sec.Name, &hosts, &sec.Sealed, &created, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(hosts), &sec.Hosts); err != nil {
			return nil, err
		}
		sec.CreatedAt, sec.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
		out = append(out, sec)
	}
	return out, rows.Err()
}

// APIKey is a caller's key; only its token's hash is stored.
type APIKey struct {
	ID        string
	Name      string
	Scope     string
	Hash      string
	Own       bool // may use only the workspaces it made
	CreatedAt time.Time
	RevokedAt time.Time // zero while it works
}

// InsertAPIKey records a new key.
func (s *Store) InsertAPIKey(ctx context.Context, k APIKey) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys (id, name, scope, hash, own, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.Scope, k.Hash, k.Own, k.CreatedAt.UnixNano())
	return err
}

// APIKeyByHash returns the key with this token hash, revoked or not.
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (APIKey, error) {
	return s.oneKey(ctx, `WHERE hash = ?`, hash)
}

// GetAPIKey returns a key by ID.
func (s *Store) GetAPIKey(ctx context.Context, id string) (APIKey, error) {
	return s.oneKey(ctx, `WHERE id = ?`, id)
}

func (s *Store) oneKey(ctx context.Context, where string, arg any) (APIKey, error) {
	list, err := s.keys(ctx, where, arg)
	if err != nil {
		return APIKey{}, err
	}
	if len(list) == 0 {
		return APIKey{}, ErrNotFound
	}
	return list[0], nil
}

// ListAPIKeys returns every key, oldest first.
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	return s.keys(ctx, `ORDER BY created_at`)
}

// RevokeAPIKey marks a key revoked (idempotent).
func (s *Store) RevokeAPIKey(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, at.UnixNano(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetAPIKey(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) keys(ctx context.Context, rest string, args ...any) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, scope, hash, own, created_at, revoked_at FROM api_keys `+rest, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var created, revoked int64
		if err := rows.Scan(&k.ID, &k.Name, &k.Scope, &k.Hash, &k.Own, &created, &revoked); err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(0, created)
		if revoked != 0 {
			k.RevokedAt = time.Unix(0, revoked)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
