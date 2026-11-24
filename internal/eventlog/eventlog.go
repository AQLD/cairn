// Package eventlog keeps each workspace's activity log: what was run in it,
// what was written, saved, resumed, copied and paused, so a person can see
// what an agent did (and watch it live).
//
// The log is observational. It lives in its own database (events.db, next to
// state.db) so it never slows or bloats the state that recovery depends on:
// recording is a non-blocking hand-off to one background writer, written with
// synchronous=NORMAL, and a crash may lose the last few events, never state.
// A damaged log is set aside and a new one started; the engine always runs.
package eventlog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Event is one thing that happened to a workspace.
type Event struct {
	// ID increases with every event the engine records, across workspaces, so
	// it is a cursor: list or follow "after" the last ID seen.
	ID        int64     `json:"id"`
	Workspace string    `json:"workspace_id"`
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	RequestID string    `json:"request_id,omitempty"`
	// Key is who acted: an API key's ID, "operator" for the engine's own
	// token, "" when the engine runs without authentication.
	Key string `json:"key,omitempty"`
	// Summary is one readable line ("exec: pytest -q → exit 1 in 2.3s").
	Summary string `json:"summary"`
	// Error is set when the operation failed.
	Error string         `json:"error,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// Defaults for Options.
const (
	DefaultPerWorkspace = 10000
	queueSize           = 4096
	batchSize           = 256
	trimEvery           = 100 // inserts per workspace between trims
)

type Options struct {
	// PerWorkspace bounds the events kept per workspace; older ones are
	// dropped (DefaultPerWorkspace if 0).
	PerWorkspace int
	Logger       *slog.Logger
	// WriteDelay holds each batch back before it is written (tests only: it
	// makes "a read sees what finished before it" observable).
	WriteDelay time.Duration
}

// Log is a workspace activity log. Record never blocks; everything else is
// safe for concurrent use.
type Log struct {
	db   *sql.DB
	opts Options
	log  *slog.Logger

	queue chan Event
	done  chan struct{}

	mu       sync.Mutex
	changed  chan struct{} // closed and replaced after each committed batch
	inserted map[string]int
	dropped  int64
	closed   bool
	lastID   int64 // highest committed ID
	pending  int   // queued and not yet written
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	workspace  TEXT NOT NULL,
	time       INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	request_id TEXT NOT NULL DEFAULT '',
	summary    TEXT NOT NULL DEFAULT '',
	error      TEXT NOT NULL DEFAULT '',
	data       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_by_workspace ON events (workspace, id);
-- when a workspace was deleted: its log is kept for the retention window
CREATE TABLE IF NOT EXISTS ended (
	workspace TEXT PRIMARY KEY,
	at        INTEGER NOT NULL
);
`

// migrateV2 adds who acted (the API key) to every event.
const migrateV2 = `
ALTER TABLE events ADD COLUMN key TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS events_by_key ON events (key, id);
PRAGMA user_version = 2;
`

// Open opens or creates the log at path. A log that cannot be opened (damaged,
// or from a newer engine) is renamed aside with a warning and a new one made.
func Open(path string, opts Options) (*Log, error) {
	if opts.PerWorkspace <= 0 {
		opts.PerWorkspace = DefaultPerWorkspace
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	db, err := open(path)
	if err != nil {
		aside := fmt.Sprintf("%s.unreadable-%d", path, time.Now().Unix())
		opts.Logger.Warn("activity log unreadable; starting a new one", "path", path, "kept_as", aside, "err", err)
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Rename(path+suffix, aside+suffix)
		}
		if db, err = open(path); err != nil {
			return nil, err
		}
	}
	l := &Log{db: db, opts: opts, log: opts.Logger, queue: make(chan Event, queueSize), done: make(chan struct{}),
		changed: make(chan struct{}), inserted: map[string]int{}}
	_ = db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&l.lastID)
	go l.writer()
	return l, nil
}

func open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		db.Close()
		return nil, err
	}
	if v > 2 {
		db.Close()
		return nil, fmt.Errorf("activity log is version %d, newer than this engine", v)
	}
	var check string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil || check != "ok" {
		db.Close()
		if err == nil {
			err = errors.New(check)
		}
		return nil, fmt.Errorf("activity log is damaged: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if v < 2 {
		if _, err := db.Exec(migrateV2); err != nil {
			db.Close()
			return nil, fmt.Errorf("activity log: add keys: %w", err)
		}
	}
	return db, nil
}

// Record queues an event. It never blocks the operation it describes: if the
// writer has fallen far behind, the event is dropped and counted.
func (l *Log) Record(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	// under mu so Close cannot close the queue between the check and the send
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	select {
	case l.queue <- ev:
		l.pending++
	default:
		l.dropped++
		if l.dropped == 1 || l.dropped%1000 == 0 {
			l.log.Warn("activity log is behind; events dropped", "dropped", l.dropped)
		}
	}
}

// Dropped is how many events were dropped because the writer was behind.
func (l *Log) Dropped() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

func (l *Log) writer() {
	defer close(l.done)
	for ev := range l.queue {
		batch := []Event{ev}
	more:
		for len(batch) < batchSize {
			select {
			case ev, ok := <-l.queue:
				if !ok {
					break more
				}
				batch = append(batch, ev)
			default:
				break more
			}
		}
		if err := l.write(batch); err != nil {
			l.log.Error("could not write activity log", "events", len(batch), "err", err)
		}
		l.mu.Lock()
		l.pending -= len(batch)
		l.mu.Unlock()
	}
}

func (l *Log) write(batch []Event) error {
	if l.opts.WriteDelay > 0 {
		time.Sleep(l.opts.WriteDelay)
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last int64
	for _, ev := range batch {
		data := ""
		if len(ev.Data) > 0 {
			b, err := json.Marshal(ev.Data)
			if err != nil {
				b, _ = json.Marshal(map[string]any{"unencodable": err.Error()})
			}
			data = string(b)
		}
		res, err := tx.Exec(`INSERT INTO events (workspace, time, kind, request_id, summary, error, data, key) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.Workspace, ev.Time.UnixNano(), ev.Kind, ev.RequestID, ev.Summary, ev.Error, data, ev.Key)
		if err != nil {
			return err
		}
		if last, err = res.LastInsertId(); err != nil {
			return err
		}
	}
	// keep each workspace's newest PerWorkspace events
	for _, ws := range l.dueForTrim(batch) {
		if _, err := tx.Exec(`DELETE FROM events WHERE workspace = ? AND id <= (
			SELECT id FROM events WHERE workspace = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`, ws, ws, l.opts.PerWorkspace); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	l.mu.Lock()
	l.lastID = last
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
	return nil
}

// dueForTrim counts inserts per workspace and returns the ones to trim now.
// Trimming every trimEvery inserts (a tenth of the bound for small bounds) lets
// a log run up to that far over it.
func (l *Log) dueForTrim(batch []Event) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	step := max(1, min(trimEvery, l.opts.PerWorkspace/10))
	var due []string
	for _, ev := range batch {
		l.inserted[ev.Workspace]++
		if l.inserted[ev.Workspace] >= step {
			due = append(due, ev.Workspace)
			l.inserted[ev.Workspace] = 0
		}
	}
	return due
}

// Query selects events of one workspace.
type Query struct {
	After int64  // only events with a larger ID
	Limit int    // at most this many (100 if 0, at most 1000)
	Kind  string // only this kind ("" for all; a prefix ending in "*" matches kinds starting with it)
	// Tail returns the newest Limit events (still oldest first) instead of the
	// oldest ones after After.
	Tail bool
	// Key, if set, keeps only what this key did (see Event.Key).
	Key string
}

const maxLimit = 1000

// AllWorkspaces makes List read every workspace's events, and the engine's
// own (keys, secrets), for an audit.
const AllWorkspaces = "*"

// List returns events oldest first.
func (l *Log) List(ctx context.Context, workspace string, q Query) ([]Event, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	q.Limit = min(q.Limit, maxLimit)
	where := `id > ?`
	args := []any{q.After}
	if workspace != AllWorkspaces {
		where += ` AND workspace = ?`
		args = append(args, workspace)
	}
	if q.Key != "" {
		where += ` AND key = ?`
		args = append(args, q.Key)
	}
	if q.Kind != "" {
		if p, ok := strings.CutSuffix(q.Kind, "*"); ok {
			where += ` AND kind LIKE ? ESCAPE '\'`
			args = append(args, strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(p)+"%")
		} else {
			where += ` AND kind = ?`
			args = append(args, q.Kind)
		}
	}
	order := `ASC`
	if q.Tail {
		order = `DESC`
	}
	rows, err := l.db.QueryContext(ctx, `SELECT id, workspace, time, kind, request_id, summary, error, data, key FROM events WHERE `+
		where+` ORDER BY id `+order+` LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var ev Event
		var t int64
		var data string
		if err := rows.Scan(&ev.ID, &ev.Workspace, &t, &ev.Kind, &ev.RequestID, &ev.Summary, &ev.Error, &data, &ev.Key); err != nil {
			return nil, err
		}
		ev.Time = time.Unix(0, t).UTC()
		if data != "" {
			_ = json.Unmarshal([]byte(data), &ev.Data)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if q.Tail {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// Last is the ID of the newest committed event of any workspace.
func (l *Log) Last() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastID
}

// Changed returns a channel closed when an event (of any workspace) newer than
// after is committed, at once if there is one. A follower takes Last, lists,
// then waits on Changed of what it took.
func (l *Log) Changed(after int64) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastID > after || l.closed {
		c := make(chan struct{})
		close(c)
		return c
	}
	return l.changed
}

// Ended records that a workspace was deleted; Purge drops its log later.
func (l *Log) Ended(ctx context.Context, workspace string, at time.Time) error {
	_, err := l.db.ExecContext(ctx, `INSERT OR REPLACE INTO ended (workspace, at) VALUES (?, ?)`, workspace, at.UnixNano())
	return err
}

// Purge drops the logs of workspaces deleted before cutoff.
func (l *Log) Purge(ctx context.Context, cutoff time.Time) (int, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT workspace FROM ended WHERE at < ?`, cutoff.UnixNano())
	if err != nil {
		return 0, err
	}
	var gone []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			rows.Close()
			return 0, err
		}
		gone = append(gone, ws)
	}
	rows.Close()
	for _, ws := range gone {
		if _, err := l.db.ExecContext(ctx, `DELETE FROM events WHERE workspace = ?`, ws); err != nil {
			return 0, err
		}
		if _, err := l.db.ExecContext(ctx, `DELETE FROM ended WHERE workspace = ?`, ws); err != nil {
			return 0, err
		}
	}
	return len(gone), nil
}

// Flush waits until everything recorded so far is written (or ctx ends).
func (l *Log) Flush(ctx context.Context) {
	for {
		l.mu.Lock()
		n := l.pending
		l.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Close writes what is queued and closes the log. Followers are released.
func (l *Log) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.queue)
	l.mu.Unlock()
	<-l.done
	l.mu.Lock()
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
	return l.db.Close()
}
