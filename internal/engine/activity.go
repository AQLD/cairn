package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AQLD/cairn/internal/eventlog"
	"github.com/AQLD/cairn/internal/redact"
	"github.com/AQLD/cairn/internal/store"
)

// The activity log records what callers did to each workspace (see
// internal/eventlog). It is observational: events are written after the
// operation, never before, and a lost event loses nothing but the record, so
// the "state before action" rule does not apply to it. Operations the engine
// does on its own (pausing idle workspaces, pausing for shutdown, waking) are
// recorded too, so a log explains why a workspace changed state.
//
// Secrets are redacted before an event is stored (internal/redact): every
// string in an event passes through note, and commands and code are redacted
// before they are cut short, so a clipped secret cannot slip past. Redaction
// is best-effort; output tails are still workspace contents.

type Event = eventlog.Event
type EventQuery = eventlog.Query

const (
	// tailBytes of each output stream, and of code and commands, are kept per event.
	tailBytes = 4 << 10
)

type requestIDKey struct{}

// WithRequestID tags ctx with the API request it serves, so events name it.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func (e *Engine) note(ctx context.Context, ws, kind, summary string, err error, data map[string]any) {
	if e.events == nil {
		return
	}
	ev := Event{Workspace: ws, Time: e.cfg.Now().UTC(), Kind: kind, Summary: redact.String(summary)}
	if data != nil {
		ev.Data = redact.Any(data).(map[string]any)
	}
	ev.RequestID, _ = ctx.Value(requestIDKey{}).(string)
	if c, ok := CallerOf(ctx); ok {
		ev.Key = c.KeyID
		if ev.Key == "" {
			ev.Key = OperatorKey
		}
	}
	if err != nil {
		// a request for a workspace that does not exist leaves no log behind
		if _, gerr := e.store.GetWorkspace(context.WithoutCancel(ctx), ws); errors.Is(gerr, store.ErrNotFound) {
			return
		}
		msg := redact.String(err.Error())
		ev.Error = msg
		ev.Summary += " failed: " + clip(msg, 200)
	}
	e.events.Record(ev)
}

// OperatorKey stands for the engine's own token in the audit log.
const OperatorKey = "operator"

// Audit lists what was done across every workspace and the engine itself
// (keys, secrets), optionally only by one key, oldest first.
func (e *Engine) Audit(ctx context.Context, q EventQuery) ([]Event, error) {
	if e.events == nil {
		return nil, fmt.Errorf("%w: the activity log is not available", ErrUnsupported)
	}
	fctx, cancel := context.WithTimeout(ctx, time.Second)
	e.events.Flush(fctx)
	cancel()
	return e.events.List(ctx, eventlog.AllWorkspaces, q)
}

// AllEvents lists events across the workspaces the caller may see and the
// engine's own (disk pressure; key and secret changes for admins only),
// oldest first. It returns how far it read (the cursor to continue from),
// which may be past the last event returned when some were not the
// caller's.
func (e *Engine) AllEvents(ctx context.Context, q EventQuery) ([]Event, int64, error) {
	evs, err := e.Audit(ctx, q)
	if err != nil {
		return nil, q.After, err
	}
	c, ok := CallerOf(ctx)
	cursor := q.After
	out := evs[:0]
	owners := map[string]string{}
	for _, ev := range evs {
		cursor = ev.ID
		switch {
		case ev.Workspace == "" && (strings.HasPrefix(ev.Kind, "key_") || strings.HasPrefix(ev.Kind, "secret_")):
			if ok && c.Scope != ScopeAdmin {
				continue
			}
		case ev.Workspace != "" && ok && c.Own:
			o, seen := owners[ev.Workspace]
			if !seen {
				if w, err := e.store.GetWorkspace(ctx, ev.Workspace); err == nil {
					o = w.Owner
				}
				owners[ev.Workspace] = o
			}
			if !c.Mine(o) {
				continue
			}
		}
		out = append(out, ev)
	}
	return out, cursor, nil
}

// Events lists a workspace's activity, oldest first. It first waits (briefly)
// for events still being written, so a caller sees every operation that
// finished before it asked. A deleted workspace's log is kept for the
// retention window.
func (e *Engine) Events(ctx context.Context, id string, q EventQuery) ([]Event, error) {
	if _, err := e.store.GetWorkspace(ctx, id); err != nil {
		return nil, err
	}
	fctx, cancel := context.WithTimeout(ctx, time.Second)
	e.events.Flush(fctx)
	cancel()
	return e.events.List(ctx, id, q)
}

// EventsLast is the newest event ID of any workspace, and EventsChanged is
// closed once there is a newer one: a follower takes EventsLast, lists its
// workspace's events, then waits on EventsChanged(last).
func (e *Engine) EventsLast() int64                         { return e.events.Last() }
func (e *Engine) EventsChanged(after int64) <-chan struct{} { return e.events.Changed(after) }

// FlushEvents waits until every recorded event can be listed (tests, shutdown).
func (e *Engine) FlushEvents(ctx context.Context) { e.events.Flush(ctx) }

func (e *Engine) openEvents() error {
	l, err := eventlog.Open(filepath.Join(e.cfg.DataDir, "events.db"), eventlog.Options{PerWorkspace: e.cfg.EventsPerWorkspace, Logger: e.log, WriteDelay: e.cfg.EventsWriteDelay})
	if err != nil {
		return fmt.Errorf("activity log: %w", err)
	}
	e.events = l
	return nil
}

// tail keeps the last tailBytes written and counts them all.
type tail struct {
	buf []byte
	n   int64
}

func (t *tail) Write(p []byte) (int, error) {
	t.n += int64(len(p))
	if len(p) >= tailBytes {
		t.buf = append(t.buf[:0], p[len(p)-tailBytes:]...)
	} else {
		t.buf = append(t.buf, p...)
		if over := len(t.buf) - tailBytes; over > 0 {
			t.buf = append(t.buf[:0], t.buf[over:]...)
		}
	}
	return len(p), nil
}

func (t *tail) String() string { return text(t.buf) }

// text is b as valid UTF-8 (a tail may start mid-character; output may be binary).
func text(b []byte) string {
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return s
}

// clip shortens s to n bytes on a character boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// commandLine is how a command reads in a summary: the script of `sh -c`,
// else the arguments joined.
func commandLine(argv []string) string {
	if len(argv) >= 3 && (argv[0] == "sh" || argv[0] == "bash" || strings.HasSuffix(argv[0], "/sh") || strings.HasSuffix(argv[0], "/bash")) && argv[1] == "-c" {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

// boundedArgs keeps a command's arguments within tailBytes in total.
func boundedArgs(argv []string) []string {
	out := make([]string, 0, len(argv))
	left := tailBytes
	for _, a := range argv {
		if left <= 0 {
			out = append(out, "…")
			break
		}
		a = clip(a, left)
		left -= len(a)
		out = append(out, a)
	}
	return out
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, 120)
}

func seconds(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func ended(code int, timedOut bool) string {
	if timedOut {
		return "timed out"
	}
	return fmt.Sprintf("exit %d", code)
}
