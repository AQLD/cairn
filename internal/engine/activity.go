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
	"github.com/AQLD/cairn/internal/store"
)

// The activity log records what callers did to each workspace (see
// internal/eventlog). It is observational: events are written after the
// operation, never before, and a lost event loses nothing but the record, so
// the "state before action" rule does not apply to it. Operations the engine
// does on its own (pausing idle workspaces, pausing for shutdown, waking) are
// recorded too, so a log explains why a workspace changed state.

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
	ev := Event{Workspace: ws, Time: e.cfg.Now().UTC(), Kind: kind, Summary: summary, Data: data}
	ev.RequestID, _ = ctx.Value(requestIDKey{}).(string)
	if err != nil {
		// a request for a workspace that does not exist leaves no log behind
		if _, gerr := e.store.GetWorkspace(context.WithoutCancel(ctx), ws); errors.Is(gerr, store.ErrNotFound) {
			return
		}
		ev.Error = err.Error()
		ev.Summary += " failed: " + clip(err.Error(), 200)
	}
	e.events.Record(ev)
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
