package acceptance

import (
	"fmt"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G19.3: an interactive terminal in a running workspace.

// screen collects a terminal's output.
type screen struct{ SyncBuffer }

// waitFor waits until the output holds s.
func (sc *screen) waitFor(e *Env, s string) {
	e.T.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(sc.String(), s) {
		if time.Now().After(deadline) {
			e.T.Fatalf("the terminal never showed %q; it showed:\n%s", s, sc.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func openTerm(e *Env, c *client.Client, ws string, rows, cols int) (*client.Terminal, *screen) {
	e.T.Helper()
	sc := &screen{}
	t, err := c.OpenTerminal(e.Ctx, ws, rows, cols, sc)
	e.Must(err)
	e.T.Cleanup(func() { t.Close() })
	return t, sc
}

func typeLine(e *Env, t *client.Terminal, line string) {
	e.T.Helper()
	_, err := t.Write([]byte(line + "\r"))
	e.Must(err)
}

func init() {
	register("G19.3.1", func(e *Env) {
		ws := e.Workspace("interactive")
		e.Write(ws.ID, "notes.txt", "from-the-api")
		t, sc := openTerm(e, e.C, ws.ID, 24, 80)
		typeLine(e, t, "[ -t 0 ] && [ -t 1 ] && echo has-$((6*7))-tty")
		sc.waitFor(e, "has-42-tty")
		typeLine(e, t, "cat /notes.txt; echo")
		sc.waitFor(e, "from-the-api")
		typeLine(e, t, "exit 7")
		select {
		case <-t.Done():
		case <-time.After(30 * time.Second):
			e.T.Fatal("the terminal did not end 30 s after its shell exited")
		}
		code, err := t.Wait()
		e.Must(err)
		Equal(e, "exit code", code, 7)
	})

	register("G19.3.2", func(e *Env) {
		ws := e.Workspace("sized")
		t, sc := openTerm(e, e.C, ws.ID, 33, 101)
		typeLine(e, t, "echo size:$(stty size)")
		sc.waitFor(e, "size:33 101")
		e.Must(t.Resize(50, 140))
		time.Sleep(300 * time.Millisecond)
		typeLine(e, t, "echo size:$(stty size)")
		sc.waitFor(e, "size:50 140")
	})

	register("G19.3.3", func(e *Env) {
		ws := e.Workspace("closing")
		t, sc := openTerm(e, e.C, ws.ID, 24, 80)
		typeLine(e, t, "nohup sh -c 'while :; do sleep 1; done' >/dev/null 2>&1 & echo kept=$!")
		sc.waitFor(e, "kept=")
		typeLine(e, t, "echo shell=$$; sleep 600")
		sc.waitFor(e, "shell=")
		out := sc.String()
		kept := field(out, "kept=")
		shell := field(out, "shell=")
		e.Must(t.Close())
		// closing travels: the API sees the WebSocket end, closes the
		// terminal's input, cairn-init's client sees it and hangs up, and
		// process 1 ends the shell's session; on a loaded host that takes a while
		deadline := time.Now().Add(30 * time.Second)
		for {
			alive := strings.Contains(e.Sh(ws.ID, "ps -o pid= -p "+shell+" 2>/dev/null || true"), shell)
			job := strings.TrimSpace(e.Sh(ws.ID, "ps -eo args | grep '^sleep 600' | grep -vc grep || true")) != "0"
			if !alive && !job {
				break
			}
			if time.Now().After(deadline) {
				e.T.Fatalf("30 s after its terminal closed: shell running %v, its foreground job running %v", alive, job)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !strings.Contains(e.Sh(ws.ID, "kill -0 "+kept+" && echo alive || echo gone"), "alive") {
			e.T.Fatal("a nohup'd process started from the terminal did not survive it")
		}
		evs, err := e.C.Events(e.Ctx, ws.ID, client.EventQuery{})
		e.Must(err)
		var kinds []string
		for _, ev := range evs {
			kinds = append(kinds, ev.Kind)
			// (this test's own API commands are recorded, as they should be)
			if strings.HasPrefix(ev.Kind, "terminal") && (strings.Contains(fmtEvent(ev), "sleep 600") || strings.Contains(fmtEvent(ev), "nohup")) {
				e.T.Fatalf("the activity log records what was typed: %+v", ev)
			}
		}
		all := strings.Join(kinds, ",")
		if !strings.Contains(all, "terminal_open") || !strings.Contains(all, "terminal_close") {
			e.T.Fatalf("the activity log does not record the terminal: %s", all)
		}
	})

	register("G19.3.4", func(e *Env) {
		withAuth(e)
		url, _ := e.endpoint()
		reader, _ := keyClient(e, "dashboard", "read")
		ownA, err := e.C.CreateOwnKey(e.Ctx, "agent-a", "write")
		e.Must(err)
		a := client.New(url, ownA.Token)
		ownB, err := e.C.CreateOwnKey(e.Ctx, "agent-b", "write")
		e.Must(err)
		b := client.New(url, ownB.Token)
		wa, err := a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "a"})
		e.Must(err)
		_, err = reader.OpenTerminal(e.Ctx, wa.ID, 24, 80, &screen{})
		e.ErrCode(err, "forbidden")
		_, err = b.OpenTerminal(e.Ctx, wa.ID, 24, 80, &screen{})
		e.ErrCode(err, "not_found")
		t, sc := openTerm(e, a, wa.ID, 24, 80)
		typeLine(e, t, "echo mine-$((1+1))")
		sc.waitFor(e, "mine-2")
	})
}

// field returns the number after a marker in terminal output.
func field(out, marker string) string {
	i := strings.LastIndex(out, marker)
	if i < 0 {
		return ""
	}
	rest := out[i+len(marker):]
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func fmtEvent(ev client.Event) string { return fmt.Sprintf("%s %v", ev.Summary, ev.Data) }
