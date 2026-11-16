package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/spec"
)

// eventsOf lists every event of a workspace (all pages), optionally of one kind.
func eventsOf(e *Env, ws, kind string) []client.Event {
	e.T.Helper()
	var all []client.Event
	q := client.EventQuery{Kind: kind, Limit: 1000}
	for {
		page, err := e.C.Events(e.Ctx, ws, q)
		e.Must(err)
		all = append(all, page...)
		if len(page) < 1000 {
			return all
		}
		q.After = page[len(page)-1].ID
	}
}

func kinds(evs []client.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.Kind
	}
	return out
}

func dataString(ev client.Event, key string) string {
	s, _ := ev.Data[key].(string)
	return s
}

func dataNumber(ev client.Event, key string) float64 {
	n, _ := ev.Data[key].(float64)
	return n
}

func mustKinds(e *Env, what string, evs []client.Event, want ...string) {
	e.T.Helper()
	if got := strings.Join(kinds(evs), " "); got != strings.Join(want, " ") {
		e.T.Fatalf("%s: kinds %q, want %q", what, got, strings.Join(want, " "))
	}
}

func init() {
	register("G10.13.1", func(e *Env) {
		ws := e.Workspace("logged")
		// a command that writes more than one tail's worth, then fails
		r, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{
			Command: []string{"sh", "-c", `i=0; while [ $i -lt 2000 ]; do echo "line $i"; i=$((i+1)); done; echo "the end"; echo "it broke" >&2; exit 3`},
			Env:     map[string]string{"API_TOKEN": "s3cret-value-123"},
		})
		e.Must(err)
		Equal(e, "exit", r.ExitCode, 3)
		e.Write(ws.ID, "notes/plan.md", "step one\n")
		got := e.Read(ws.ID, "notes/plan.md")
		_, missErr := e.C.ReadFile(e.Ctx, ws.ID, "no/such/file")
		var apiErr *client.Error
		if !errors.As(missErr, &apiErr) || apiErr.RequestID == "" {
			e.T.Fatalf("reading a missing file: %v", missErr)
		}
		e.Sh(ws.ID, "true")

		evs := eventsOf(e, ws.ID, "")
		mustKinds(e, "log", evs, "create", "exec", "file_write", "file_read", "file_read", "exec")
		ex := evs[1]
		Equal(e, "exec exit code", dataNumber(ex, "exit_code"), 3)
		Equal(e, "stdout bytes", int(dataNumber(ex, "stdout_bytes")), len(r.Stdout))
		Equal(e, "stderr bytes", int(dataNumber(ex, "stderr_bytes")), len("it broke\n"))
		tail := dataString(ex, "stdout_tail")
		if !strings.HasSuffix(tail, "line 1999\nthe end\n") || len(tail) > 4096 || len(tail) < 2048 {
			t := tail
			if len(t) > 80 {
				t = "…" + t[len(t)-80:]
			}
			e.T.Fatalf("stdout tail (%d bytes) should be the last 4 KiB of the output, ending %q", len(tail), t)
		}
		Equal(e, "stderr tail", dataString(ex, "stderr_tail"), "it broke\n")
		if !strings.Contains(ex.Summary, "exit 3") || !strings.Contains(ex.Summary, `echo "the end"`) {
			e.T.Fatalf("exec summary %q should show the command and its exit code", ex.Summary)
		}
		if _, ok := ex.Data["duration_ms"].(float64); !ok {
			e.T.Fatalf("exec event has no duration: %v", ex.Data)
		}
		raw, _ := json.Marshal(evs)
		if strings.Contains(string(raw), "s3cret-value-123") {
			e.T.Fatal("an environment variable's value was written to the activity log")
		}
		if env, _ := ex.Data["env"].([]any); len(env) != 1 || env[0] != "API_TOKEN" {
			e.T.Fatalf("exec event should list the environment's names only: %v", ex.Data["env"])
		}
		Equal(e, "write path", dataString(evs[2], "path"), "notes/plan.md")
		Equal(e, "write bytes", int(dataNumber(evs[2], "bytes")), len("step one\n"))
		Equal(e, "read bytes", int(dataNumber(evs[3], "bytes")), len(got))
		miss := evs[4]
		if miss.Error == "" || dataString(miss, "path") != "no/such/file" {
			e.T.Fatalf("a failed read should be logged with its error: %+v", miss)
		}
		Equal(e, "failed read names its request", miss.RequestID, apiErr.RequestID)
		for _, ev := range evs {
			if ev.RequestID == "" || ev.WorkspaceID != ws.ID || ev.Time.IsZero() || ev.Summary == "" {
				t, _ := json.Marshal(ev)
				e.T.Fatalf("event lacks its request, workspace, time or summary: %s", t)
			}
		}
		for i := 1; i < len(evs); i++ {
			if evs[i].ID <= evs[i-1].ID {
				e.T.Fatalf("event IDs must increase: %d then %d", evs[i-1].ID, evs[i].ID)
			}
		}
		// filters and paging
		Equal(e, "only commands", len(eventsOf(e, ws.ID, "exec")), 2)
		Equal(e, "a kind prefix", len(eventsOf(e, ws.ID, "file_*")), 3)
		last, err := e.C.Events(e.Ctx, ws.ID, client.EventQuery{Last: 2})
		e.Must(err)
		mustKinds(e, "the last two", last, "file_read", "exec")
		after, err := e.C.Events(e.Ctx, ws.ID, client.EventQuery{After: evs[3].ID})
		e.Must(err)
		mustKinds(e, "after a cursor", after, "file_read", "exec")
		// a workspace that never existed has no log
		_, err = e.C.Events(e.Ctx, "ws_0000000000000000", client.EventQuery{})
		e.ErrCode(err, "not_found")
		_, err = e.C.Exec(e.Ctx, "ws_0000000000000000", client.Exec{Command: []string{"true"}})
		e.ErrCode(err, "not_found")
		_, err = e.C.Events(e.Ctx, "ws_0000000000000000", client.EventQuery{})
		e.ErrCode(err, "not_found")
	})

	register("G10.13.2", func(e *Env) {
		ws := e.Workspace("watched")
		e.Sh(ws.ID, "echo before")
		ctx, stop := context.WithCancel(e.Ctx)
		got := make(chan client.Event, 100)
		done := make(chan error, 1)
		go func() {
			done <- e.C.FollowEvents(ctx, ws.ID, client.EventQuery{}, func(ev client.Event) error {
				got <- ev
				return nil
			})
		}()
		next := func(what string) client.Event {
			e.T.Helper()
			select {
			case ev := <-got:
				return ev
			case err := <-done:
				e.T.Fatalf("follow ended before %s: %v", what, err)
			case <-time.After(10 * time.Second):
				e.T.Fatalf("no event for %s within 10s", what)
			}
			return client.Event{}
		}
		Equal(e, "the existing first", next("the create").Kind, "create")
		Equal(e, "then the earlier command", dataString(next("the earlier command"), "stdout_tail"), "before\n")
		// a busy neighbour's events must not show up, nor stall this follower
		other := e.Workspace("neighbour")
		for i := 0; i < 3; i++ {
			e.Sh(other.ID, "true")
		}
		for i := 0; i < 5; i++ {
			start := time.Now()
			e.Sh(ws.ID, fmt.Sprintf("echo live-%d", i))
			ev := next(fmt.Sprintf("command %d", i))
			Equal(e, "live event", dataString(ev, "stdout_tail"), fmt.Sprintf("live-%d\n", i))
			if ev.WorkspaceID != ws.ID {
				e.T.Fatalf("a follower saw another workspace's event: %+v", ev)
			}
			if d := time.Since(start); d > 3*time.Second {
				e.T.Fatalf("command %d took %v to reach the follower", i, d)
			}
		}
		// many at once: all arrive, in order, past one page
		for i := 0; i < 120; i++ {
			e.Write(ws.ID, "burst", fmt.Sprint(i))
		}
		for i := 0; i < 120; i++ {
			ev := next(fmt.Sprintf("burst write %d", i))
			Equal(e, "burst event", ev.Kind, "file_write")
			Equal(e, "burst order", int(dataNumber(ev, "bytes")), len(fmt.Sprint(i)))
		}
		stop()
		select {
		case err := <-done:
			if err != nil {
				e.T.Fatalf("stopping a follow: %v", err)
			}
		case <-time.After(5 * time.Second):
			e.T.Fatal("follow did not end when its caller stopped")
		}

		// a follower that starts behind a backlog longer than one page gets
		// all of it without waiting for anything new to happen
		quiet := e.Workspace("backlog")
		for i := 0; i < 250; i++ {
			e.Write(quiet.ID, "b", "x")
		}
		bctx, bstop := context.WithTimeout(e.Ctx, 10*time.Second)
		defer bstop()
		n := 0
		err := e.C.FollowEvents(bctx, quiet.ID, client.EventQuery{}, func(client.Event) error {
			if n++; n == 251 {
				bstop()
			}
			return nil
		})
		e.Must(err)
		Equal(e, "the whole backlog arrived", n, 251)
	})

	register("G10.13.3", func(e *Env) {
		ws := e.Workspace("original")
		e.Write(ws.ID, "f", "v1")
		sv := e.Save(ws.ID)
		e.Write(ws.ID, "f", "v2")
		later := e.Save(ws.ID)
		e.Sh(ws.ID, "echo broke-it > f")
		e.Resume(sv.ID) // rewind past `later`
		Equal(e, "rewound", e.Read(ws.ID, "f"), "v1")

		evs := eventsOf(e, ws.ID, "")
		mustKinds(e, "the original's log keeps everything before the rewind", evs,
			"create", "file_write", "save", "file_write", "save", "exec", "resume", "file_read")
		Equal(e, "save event names its save", dataString(evs[2], "save"), sv.ID)
		Equal(e, "second save names its parent", dataString(evs[4], "parent"), sv.ID)
		res := evs[6]
		Equal(e, "resume names the save", dataString(res, "save"), sv.ID)
		Equal(e, "resume names where the state was", dataString(res, "from"), later.ID)

		copies, err := e.C.Copy(e.Ctx, sv.ID, 2)
		e.Must(err)
		for _, c := range copies {
			cev := eventsOf(e, c.ID, "")
			mustKinds(e, "a copy's log starts fresh", cev, "create")
			Equal(e, "copy names its save", dataString(cev[0], "from_save"), sv.ID)
			Equal(e, "copy names its workspace", dataString(cev[0], "from_workspace"), ws.ID)
		}
		e.Sh(copies[0].ID, "echo only-here")
		Equal(e, "copies log separately", len(eventsOf(e, copies[1].ID, "")), 1)
		cp := eventsOf(e, ws.ID, "copy")
		if len(cp) != 1 || !strings.Contains(cp[0].Summary, copies[0].ID) || !strings.Contains(cp[0].Summary, copies[1].ID) {
			e.T.Fatalf("the original's log should list its copies: %+v", cp)
		}
	})

	register("G10.13.4", func(e *Env) {
		e.Need(spec.NeedInProcess)
		ws := e.Workspace("long-lived")
		e.Sh(ws.ID, "echo one")
		e.Restart()
		e.Sh(ws.ID, "echo two")
		evs := eventsOf(e, ws.ID, "exec")
		if len(evs) != 2 || dataString(evs[0], "stdout_tail") != "one\n" || dataString(evs[1], "stdout_tail") != "two\n" {
			e.T.Fatalf("events from before the restart should still be listed: %+v", evs)
		}
		if evs[1].ID <= evs[0].ID {
			e.T.Fatal("IDs after a restart must keep increasing")
		}

		// bounded: only the newest events are kept
		e.RestartWith(func(c *engine.Config) { c.EventsPerWorkspace = 50 })
		for i := 0; i < 200; i++ {
			e.Write(ws.ID, "n", fmt.Sprint(i))
		}
		all := eventsOf(e, ws.ID, "")
		if len(all) < 50 || len(all) > 60 {
			e.T.Fatalf("a workspace bounded to 50 events kept %d", len(all))
		}
		if lastEv := all[len(all)-1]; dataNumber(lastEv, "bytes") != float64(len("199")) {
			e.T.Fatalf("the newest event must be kept: %+v", lastEv)
		}

		// a deleted workspace's log is kept for the retention window
		gone := e.Workspace("short-lived")
		e.Sh(gone.ID, "echo last words")
		e.Must(e.C.DeleteWorkspace(e.Ctx, gone.ID))
		left := eventsOf(e, gone.ID, "")
		mustKinds(e, "after delete", left, "create", "exec", "delete")
		e.Clock.Advance(engine.DefaultRetention - time.Hour)
		_, err := e.Engine.PurgeExpired(e.Ctx)
		e.Must(err)
		Equal(e, "kept within the window", len(eventsOf(e, gone.ID, "")), 3)
		e.Clock.Advance(2 * time.Hour)
		_, err = e.Engine.PurgeExpired(e.Ctx)
		e.Must(err)
		after, err := e.C.Events(e.Ctx, gone.ID, client.EventQuery{})
		e.Must(err)
		Equal(e, "purged after the window", len(after), 0)
		Equal(e, "a live workspace's log is not purged", len(eventsOf(e, ws.ID, "")) > 0, true)
	})

	register("G10.13.5", func(e *Env) {
		ws := e.Workspace("sessions-and-saves")
		s := session(e, ws.ID, "python")
		cell(e, ws.ID, s.ID, "x = 6 * 7")
		Equal(e, "cell", strings.TrimSpace(cell(e, ws.ID, s.ID, "x")), "42")
		r, err := e.C.RunInSession(e.Ctx, ws.ID, s.ID, "1/0", 0)
		e.Must(err)
		Equal(e, "failing cell", r.ExitCode, 1)
		full, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		e.Resume(full.ID)
		_, err = e.C.PauseWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "woken by a cell, state intact", strings.TrimSpace(cell(e, ws.ID, s.ID, "x + 1")), "43")

		evs := eventsOf(e, ws.ID, "")
		mustKinds(e, "log", evs, "create", "session_start", "session_run", "session_run", "session_run",
			"save", "resume", "pause", "wake", "session_run")
		Equal(e, "session started", dataString(evs[1], "session"), s.ID)
		Equal(e, "cell code", dataString(evs[2], "code"), "x = 6 * 7")
		Equal(e, "cell output", dataString(evs[3], "stdout_tail"), "42\n")
		Equal(e, "failing cell exit", dataNumber(evs[4], "exit_code"), 1)
		if !strings.Contains(dataString(evs[4], "stderr_tail"), "ZeroDivisionError") {
			e.T.Fatalf("a failing cell's traceback should be in its event: %v", evs[4].Data)
		}
		Equal(e, "full save", dataString(evs[5], "kind"), "full")
		Equal(e, "full save id", dataString(evs[5], "save"), full.ID)
		Equal(e, "paused on request", dataString(evs[7], "reason"), "requested")
		if dataString(evs[7], "save") == "" {
			e.T.Fatalf("a pause names the save holding its memory: %v", evs[7].Data)
		}
		Equal(e, "woke with processes", evs[8].Data["processes"], any(true))
		// the plumbing behind sessions is not logged as commands of its own
		Equal(e, "no stray commands", len(eventsOf(e, ws.ID, "exec")), 0)
	})
}
