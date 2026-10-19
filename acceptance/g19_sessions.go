package acceptance

import (
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G19.1, G19.2: sessions, interpreters kept running in a workspace so state
// carries over between calls, as in a notebook or a terminal.

// session starts a session and fails the scenario on error.
func session(e *Env, ws, language string) *client.Session {
	e.T.Helper()
	s, err := e.C.CreateSession(e.Ctx, ws, language)
	e.Must(err)
	return s
}

// cell runs code in a session and fails unless it succeeds; it returns stdout.
func cell(e *Env, ws, ses, code string) string {
	e.T.Helper()
	r, err := e.C.RunInSession(e.Ctx, ws, ses, code, 0)
	e.Must(err)
	if r.ExitCode != 0 || r.TimedOut || r.Ended {
		e.T.Fatalf("cell %q: exit %d, timed out %v, ended %v\n%s", code, r.ExitCode, r.TimedOut, r.Ended, r.Stderr)
	}
	return r.Stdout
}

func init() {
	register("G19.1.1", func(e *Env) {
		ws := e.Workspace("python-session")
		s := session(e, ws.ID, "python")
		cell(e, ws.ID, s.ID, "import json\nrows = [{'region': r, 'total': t} for r, t in (('north', 120), ('south', -45))]")
		Equal(e, "a final expression is shown", cell(e, ws.ID, s.ID, "len(rows)"), "2\n")
		Equal(e, "imports and variables carry over", cell(e, ws.ID, s.ID, "print(json.dumps(rows[1]))"), `{"region": "south", "total": -45}`+"\n")

		// an error costs the cell, not the session
		r, err := e.C.RunInSession(e.Ctx, ws.ID, s.ID, "rows[5]", 0)
		e.Must(err)
		Equal(e, "a failing cell's exit code", r.ExitCode, 1)
		if !strings.Contains(r.Stderr, "IndexError") || !strings.Contains(r.Stderr, "rows[5]") {
			e.T.Fatalf("the traceback should name the error and the line: %q", r.Stderr)
		}
		Equal(e, "the session carries on", cell(e, ws.ID, s.ID, "rows[0]['total']"), "120\n")

		// and processes started from a cell see the same files
		cell(e, ws.ID, s.ID, "open('/root/summary.csv', 'w').write('north,120\\n')")
		Equal(e, "files the session writes", e.Read(ws.ID, "root/summary.csv"), "north,120\n")
	})

	register("G19.1.2", func(e *Env) {
		ws := e.Workspace("shell-session")
		s := session(e, ws.ID, "shell")
		cell(e, ws.ID, s.ID, "mkdir -p /root/project && cd /root/project\nexport STAGE=build\ngreet() { echo \"hello $1\"; }")
		Equal(e, "working folder, variables and functions carry over", cell(e, ws.ID, s.ID, "pwd; echo $STAGE; greet cairn"), "/root/project\nbuild\nhello cairn\n")

		r, err := e.C.RunInSession(e.Ctx, ws.ID, s.ID, "false", 0)
		e.Must(err)
		Equal(e, "a failing command's status", r.ExitCode, 1)
		r, err = e.C.RunInSession(e.Ctx, ws.ID, s.ID, "if then fi", 0)
		e.Must(err)
		Equal(e, "a syntax error's status", r.ExitCode, 2)
		Equal(e, "the session carries on after both", cell(e, ws.ID, s.ID, "pwd"), "/root/project\n")
	})

	register("G19.1.3", func(e *Env) {
		ws := e.Workspace("session-timeout")
		for _, c := range []struct{ language, setup, slow, check, want string }{
			{"python", "x = 41", "import time\nwhile True: time.sleep(0.1)", "x + 1", "42\n"},
			{"shell", "x=41", "sleep 600", "echo $((x + 1))", "42\n"},
		} {
			s := session(e, ws.ID, c.language)
			cell(e, ws.ID, s.ID, c.setup)
			start := time.Now()
			r, err := e.C.RunInSession(e.Ctx, ws.ID, s.ID, c.slow, time.Second)
			e.Must(err)
			if !r.TimedOut || r.Ended {
				e.T.Fatalf("%s: a cell past its timeout: timed out %v, ended %v", c.language, r.TimedOut, r.Ended)
			}
			if took := time.Since(start); took > 15*time.Second {
				e.T.Fatalf("%s: the interrupted cell took %v to come back", c.language, took)
			}
			Equal(e, c.language+": the session keeps its state", cell(e, ws.ID, s.ID, c.check), c.want)
		}
	})

	register("G19.1.4", func(e *Env) {
		ws := e.Workspace("session-lifecycle")
		py, sh := session(e, ws.ID, "python"), session(e, ws.ID, "shell")
		cell(e, ws.ID, sh.ID, "sleep 600 &") // started by the session
		list, err := e.C.ListSessions(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "sessions listed", len(list), 2)

		// one cell at a time per session
		done := make(chan error, 1)
		go func() { _, err := e.C.RunInSession(e.Ctx, ws.ID, py.ID, "import time; time.sleep(3)", 0); done <- err }()
		time.Sleep(time.Second)
		_, err = e.C.RunInSession(e.Ctx, ws.ID, py.ID, "1", 0)
		e.ErrCode(err, "conflict")
		e.Must(<-done)

		e.Must(e.C.DeleteSession(e.Ctx, ws.ID, sh.ID))
		Equal(e, "what the deleted session started is stopped", strings.TrimSpace(e.Sh(ws.ID, `for p in /proc/[0-9]*; do [ "$(tr '\0' ' ' < $p/cmdline 2>/dev/null)" = "sleep 600 " ] && echo $p; done; true`)), "")
		_, err = e.C.RunInSession(e.Ctx, ws.ID, sh.ID, "true", 0)
		e.ErrCode(err, "not_found")

		// a session whose process dies says so, then is gone
		r, err := e.C.RunInSession(e.Ctx, ws.ID, py.ID, "import os; os._exit(3)", 0)
		e.Must(err)
		if !r.Ended {
			e.T.Fatalf("a session that died during its cell: %+v", r)
		}
		list, err = e.C.ListSessions(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "sessions listed after both ended", len(list), 0)
	})

	register("G19.2", func(e *Env) {
		ws := e.Workspace("session-pause")
		py, sh := session(e, ws.ID, "python"), session(e, ws.ID, "shell")
		cell(e, ws.ID, py.ID, "data = list(range(1_000_000))")
		cell(e, ws.ID, sh.ID, "cd /tmp && n=7")

		full, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		e.Resume(full.ID)
		Equal(e, "the Python session after a full save", cell(e, ws.ID, py.ID, "sum(data)"), "499999500000\n")
		Equal(e, "the shell session after a full save", cell(e, ws.ID, sh.ID, "pwd; echo $n"), "/tmp\n7\n")

		_, err = e.C.PauseWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "a paused workspace wakes for its session", cell(e, ws.ID, py.ID, "len(data)"), "1000000\n")

		// a quick save keeps files only: its sessions do not come back
		quick := e.Save(ws.ID)
		e.Resume(quick.ID)
		_, err = e.C.RunInSession(e.Ctx, ws.ID, py.ID, "len(data)", 0)
		e.ErrCode(err, "not_found")
	})
}
