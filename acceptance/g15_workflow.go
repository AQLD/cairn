package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G15: a workflow agent inside a company: it stops before risky steps until a
// person decides, and runs on a schedule, each day carrying on from the last.

// approvalWorkflow drafts a change, waits for a person's decision, then
// applies the change only if it was approved. Every step is logged with the
// process that ran it.
const approvalWorkflow = `import os, time
def log(m):
    with open('workflow.log', 'a') as f:
        f.write('%s %d\n' % (m, os.getpid()))
log('drafted')
open('plan.txt', 'w').write('drop table sessions\n')
log('waiting')
while not os.path.exists('approval'):
    time.sleep(0.1)
decision = open('approval').read().strip()
log(decision)
if decision == 'approved':
    open('applied.txt', 'w').write(open('plan.txt').read())
log('done')
`

// scheduledAgent runs every second, handles the inbox items that arrived
// since its last run, and keeps its place (cursor) only in memory. Its state
// file says when it last ran and the longest gap between runs.
const scheduledAgent = `import json, os, time
cursor, runs, last, gap = 0, 0, None, 0.0
while True:
    now = time.time()
    if last is not None:
        gap = max(gap, now - last)
    last = now
    for i in sorted(int(n) for n in os.listdir('inbox')):
        if i > cursor:
            with open('processed.log', 'a') as f:
                f.write('%d %d\n' % (i, os.getpid()))
            cursor = i
    runs += 1
    json.dump({'pid': os.getpid(), 'cursor': cursor, 'runs': runs, 'gap': gap}, open('schedule.tmp', 'w'))
    os.replace('schedule.tmp', 'schedule.json')
    time.sleep(1)
`

type schedule struct {
	PID    int     `json:"pid"`
	Cursor int     `json:"cursor"`
	Runs   int     `json:"runs"`
	Gap    float64 `json:"gap"`
}

// waitFor polls until cond holds, failing after limit.
func waitFor(e *Env, what string, limit time.Duration, cond func() bool) {
	e.T.Helper()
	for deadline := time.Now().Add(limit); !cond(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			e.T.Fatalf("%s: not within %v", what, limit)
		}
	}
}

// sandboxProcesses counts the host processes running a workspace's sandbox,
// or -1 where this test cannot see the host's processes.
func sandboxProcesses(e *Env, ws string) int {
	if e.Engine == nil || e.Info.Runtime != "runsc" {
		return -1
	}
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	root, id := "--root="+filepath.Join(e.DataDir, "runsc"), "cairn-"+ws
	n := 0
	for _, p := range procs {
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), root) && strings.Contains(string(b), id) {
			n++
		}
	}
	return n
}

// waitsForFree full-saves a workspace and checks it holds no compute while it
// waits: stopped, and no sandbox left on the host.
func waitsForFree(e *Env, ws string) *client.Save {
	e.T.Helper()
	running := sandboxProcesses(e, ws)
	if running == 0 {
		e.T.Fatal("no sandbox processes found for the running workspace: this check would see nothing")
	}
	sv, err := e.C.Save(e.Ctx, ws, client.FullSave)
	e.Must(err)
	got, err := e.C.GetWorkspace(e.Ctx, ws)
	e.Must(err)
	Equal(e, "state while waiting", got.State, "stopped")
	if running > 0 {
		Equal(e, "sandbox processes while waiting", sandboxProcesses(e, ws), 0)
	}
	return sv
}

// ordersWorkflow keeps a database connection open, as a long-running workflow
// does, and answers how many orders it sees whenever count.req appears.
const ordersWorkflow = `import os, sqlite3, time
db = sqlite3.connect('orders.db', isolation_level=None)
db.execute('CREATE TABLE IF NOT EXISTS orders (id INTEGER PRIMARY KEY, total INTEGER)')
if db.execute('SELECT count(*) FROM orders').fetchone()[0] == 0:
    db.executemany('INSERT INTO orders (total) VALUES (?)', [(i * 10,) for i in range(1, 101)])
open('workflow.pid', 'w').write(str(os.getpid()))
while True:
    if os.path.exists('count.req'):
        os.remove('count.req')
        n, s = db.execute('SELECT count(*), sum(total) FROM orders').fetchone()
        open('count.tmp', 'w').write('%d %d %d' % (n, s or 0, os.getpid()))
        os.replace('count.tmp', 'count.out')
    time.sleep(0.05)
`

// orders asks the workflow process what it sees in its database.
func orders(e *Env, ws string) string {
	e.T.Helper()
	e.Sh(ws, "rm -f count.out && touch count.req")
	var out string
	waitFor(e, "the workflow answers", 20*time.Second, func() bool {
		b, err := e.C.ReadFile(e.Ctx, ws, "count.out")
		out = string(b)
		return err == nil
	})
	return out
}

func readSchedule(e *Env, ws string) schedule {
	e.T.Helper()
	var s schedule
	if b, err := e.C.ReadFile(e.Ctx, ws, "schedule.json"); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func init() {
	register("G15.3", func(e *Env) {
		ws := e.Workspace("approval")
		e.Write(ws.ID, "workflow.py", approvalWorkflow)
		background(e, ws.ID, "exec python3 workflow.py")
		waitFor(e, "the workflow reaches its approval step", 30*time.Second, func() bool {
			b, _ := e.C.ReadFile(e.Ctx, ws.ID, "workflow.log")
			return strings.Contains(string(b), "waiting")
		})
		sv := waitsForFree(e, ws.ID)
		time.Sleep(time.Second) // the person takes their time

		e.Resume(sv.ID)
		e.Write(ws.ID, "approval", "approved\n")
		waitFor(e, "the workflow finishes after approval", 30*time.Second, func() bool {
			b, _ := e.C.ReadFile(e.Ctx, ws.ID, "workflow.log")
			return strings.Contains(string(b), "done")
		})
		var steps, pids []string
		for _, line := range strings.Split(strings.TrimSpace(e.Read(ws.ID, "workflow.log")), "\n") {
			f := strings.Fields(line)
			steps, pids = append(steps, f[0]), append(pids, f[1])
		}
		Equal(e, "steps, each once", strings.Join(steps, " "), "drafted waiting approved done")
		for _, p := range pids {
			Equal(e, "one process ran every step", p, pids[0])
		}
		Equal(e, "the approved change is applied", e.Read(ws.ID, "applied.txt"), "drop table sessions\n")
	})

	register("G15.5", func(e *Env) {
		ws := e.Workspace("bad-change")
		e.Write(ws.ID, "workflow.py", ordersWorkflow)
		background(e, ws.ID, "exec python3 workflow.py")
		good := orders(e, ws.ID)
		Equal(e, "orders before the change", good, "100 50500 "+strings.TrimSpace(e.Read(ws.ID, "workflow.pid")))
		before := stepSave(e, ws.ID) // saved before the risky step

		// the bad change: a migration that deletes what it should have archived
		e.Sh(ws.ID, `python3 -c "
import sqlite3
db = sqlite3.connect('orders.db')
db.execute('DELETE FROM orders WHERE total > 100')
db.commit()
"`)
		Equal(e, "orders after the bad change", orders(e, ws.ID), "10 550 "+strings.TrimSpace(e.Read(ws.ID, "workflow.pid")))

		e.Resume(before.ID)
		Equal(e, "the same process sees every order again", orders(e, ws.ID), good)
		Equal(e, "the database is intact", strings.TrimSpace(e.Sh(ws.ID, `python3 -c "import sqlite3; print(sqlite3.connect('orders.db').execute('PRAGMA integrity_check').fetchone()[0])"`)), "ok")
	})

	register("G15.6", func(e *Env) {
		ws := e.Workspace("scheduled")
		e.Write(ws.ID, "agent.py", scheduledAgent)
		e.Sh(ws.ID, "mkdir -p inbox")
		background(e, ws.ID, "exec python3 agent.py")
		for i := 1; i <= 3; i++ {
			e.Write(ws.ID, fmt.Sprintf("inbox/%d", i), "")
		}
		waitFor(e, "today's items handled", 30*time.Second, func() bool { return readSchedule(e, ws.ID).Cursor == 3 })
		today := readSchedule(e, ws.ID)

		// end of day: saved overnight, no compute
		sv := waitsForFree(e, ws.ID)
		time.Sleep(pauseFor())
		e.Resume(sv.ID)
		for i := 4; i <= 5; i++ {
			e.Write(ws.ID, fmt.Sprintf("inbox/%d", i), "")
		}
		waitFor(e, "the next day's items handled", 30*time.Second, func() bool { return readSchedule(e, ws.ID).Cursor == 5 })
		next := readSchedule(e, ws.ID)

		Equal(e, "the same agent process", next.PID, today.PID)
		if next.Runs <= today.Runs {
			e.T.Fatalf("runs went from %d to %d: the agent started over", today.Runs, next.Runs)
		}
		if next.Gap < pauseFor().Seconds()-1 {
			e.T.Fatalf("longest gap between runs %.1fs, want about the %v pause: the agent did not see the night pass", next.Gap, pauseFor())
		}
		var handled []string
		for _, line := range strings.Split(strings.TrimSpace(e.Read(ws.ID, "processed.log")), "\n") {
			f := strings.Fields(line)
			Equal(e, "item "+f[0]+" handled by the same process", f[1], fmt.Sprint(today.PID))
			handled = append(handled, f[0])
		}
		Equal(e, "every item handled once, in order", strings.Join(handled, " "), "1 2 3 4 5")
	})
}
