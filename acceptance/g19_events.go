package acceptance

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/AQLD/cairn/client"
)

// G19.6: clients hear what happens without polling.

// listener follows /v1/events from now on and keeps what it hears.
type listener struct {
	mu   sync.Mutex
	evs  []client.Event
	stop context.CancelFunc
	done chan struct{}
}

func listen(e *Env, c *client.Client) *listener {
	e.T.Helper()
	// from just after the newest event now (read with the operator's view,
	// which sees everything), so only what happens next is heard
	var after int64
	if evs, err := e.C.AllEvents(e.Ctx, client.EventQuery{Last: 1}); err == nil && len(evs) > 0 {
		after = evs[len(evs)-1].ID
	}
	ctx, cancel := context.WithCancel(e.Ctx)
	l := &listener{stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		_ = c.FollowAllEvents(ctx, client.EventQuery{After: after}, func(ev client.Event) error {
			l.mu.Lock()
			l.evs = append(l.evs, ev)
			l.mu.Unlock()
			return nil
		})
	}()
	e.T.Cleanup(func() { cancel(); <-l.done })
	return l
}

// wait returns the first event matching, within the timeout.
func (l *listener) wait(e *Env, what string, timeout time.Duration, match func(client.Event) bool) client.Event {
	e.T.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		for _, ev := range l.evs {
			if match(ev) {
				l.mu.Unlock()
				return ev
			}
		}
		l.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e.T.Fatalf("no %s within %s; heard: %+v", what, timeout, l.evs)
	return client.Event{}
}

func (l *listener) all() []client.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]client.Event{}, l.evs...)
}

func init() {
	register("G19.6.1", func(e *Env) {
		a := e.Workspace("a")
		b := e.Workspace("b")
		e.Sh(a.ID, "echo before-listening")
		l := listen(e, e.C)
		_, err := e.C.Exec(e.Ctx, a.ID, client.Exec{Command: []string{"sh", "-c", "exit 3"}})
		e.Must(err)
		_, err = e.C.Exec(e.Ctx, b.ID, client.Exec{Command: []string{"true"}})
		e.Must(err)
		ea := l.wait(e, "a's command finishing", 10*time.Second, func(ev client.Event) bool { return ev.Kind == "exec" && ev.WorkspaceID == a.ID })
		eb := l.wait(e, "b's command finishing", 10*time.Second, func(ev client.Event) bool { return ev.Kind == "exec" && ev.WorkspaceID == b.ID })
		if code, _ := ea.Data["exit_code"].(float64); code != 3 {
			e.T.Fatalf("a's exit code on the stream: %v", ea.Data["exit_code"])
		}
		if ea.ID >= eb.ID {
			e.T.Fatalf("events out of order: %d then %d", ea.ID, eb.ID)
		}
		for _, ev := range l.all() {
			if strings.Contains(ev.Summary, "before-listening") {
				e.T.Fatal("the stream replayed an event from before it started")
			}
		}
	})

	register("G19.6.2", func(e *Env) {
		ws := e.Workspace("sleepy")
		l := listen(e, e.C)
		_, err := e.C.PauseWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		l.wait(e, "the pause", 30*time.Second, func(ev client.Event) bool { return ev.Kind == "pause" && ev.WorkspaceID == ws.ID })
		e.Sh(ws.ID, "true") // wakes it
		l.wait(e, "the wake", 30*time.Second, func(ev client.Event) bool { return ev.Kind == "wake" && ev.WorkspaceID == ws.ID })
	})

	register("G19.6.3", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "small", Limits: client.Limits{MemoryMB: 256}})
		e.Must(err)
		l := listen(e, e.C)
		_, err = e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"python3", "-c", "b = bytearray(700 * 1024 * 1024)"}, TimeoutMS: 60_000})
		e.Must(err)
		ev := l.wait(e, "a memory limit event", 20*time.Second, func(ev client.Event) bool { return ev.Kind == "limit" && ev.WorkspaceID == ws.ID })
		if ev.Data["limit"] != "memory" || !strings.Contains(ev.Summary, "memory") {
			e.T.Fatalf("limit event: %+v", ev)
		}
		if cmd, _ := ev.Data["command"].([]any); len(cmd) == 0 || cmd[0] != "python3" {
			e.T.Fatalf("the limit event does not name the command: %+v", ev.Data)
		}
	})

	register("G19.6.4", func(e *Env) {
		withAuth(e)
		mk := func(name string) *client.Client {
			k, err := e.C.CreateOwnKey(e.Ctx, name, "write")
			e.Must(err)
			url, _ := e.endpoint()
			return client.New(url, k.Token)
		}
		a, b := mk("agent-a"), mk("agent-b")
		wa, err := a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "a"})
		e.Must(err)
		wb, err := b.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "b"})
		e.Must(err)
		la := listen(e, a)
		// many of b's events first, then one of a's: a's stream must get past
		// all of b's without showing them
		for i := 0; i < 120; i++ {
			_, err := b.Exec(e.Ctx, wb.ID, client.Exec{Command: []string{"true"}})
			e.Must(err)
		}
		_, err = e.C.CreateKey(e.Ctx, "someone-else", "read")
		e.Must(err)
		_, err = a.Exec(e.Ctx, wa.ID, client.Exec{Command: []string{"echo", "mine"}})
		e.Must(err)
		la.wait(e, "a's own command", 20*time.Second, func(ev client.Event) bool { return ev.Kind == "exec" && ev.WorkspaceID == wa.ID })
		for _, ev := range la.all() {
			if ev.WorkspaceID == wb.ID || strings.HasPrefix(ev.Kind, "key_") {
				e.T.Fatalf("an own key's stream shows %s in %q", ev.Kind, ev.WorkspaceID)
			}
		}
	})

	register("G16.4", func(e *Env) {
		// a product watching its users' workspaces
		user, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "user-42", Limits: client.Limits{MemoryMB: 256}})
		e.Must(err)
		l := listen(e, e.C)
		_, err = e.C.Exec(e.Ctx, user.ID, client.Exec{Command: []string{"python3", "-c", "b = bytearray(700 * 1024 * 1024)"}, TimeoutMS: 60_000})
		e.Must(err)
		_, err = e.C.PauseWorkspace(e.Ctx, user.ID)
		e.Must(err)
		e.Sh(user.ID, "true")
		for _, kind := range []string{"limit", "pause", "wake"} {
			l.wait(e, "the "+kind, 30*time.Second, func(ev client.Event) bool { return ev.Kind == kind && ev.WorkspaceID == user.ID })
		}
	})
}
