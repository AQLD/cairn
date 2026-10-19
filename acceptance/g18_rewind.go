package acceptance

import (
	"strings"

	"github.com/AQLD/cairn/client"
)

// G18: every step can be rewound. An agent saves after each step; resuming an
// earlier save rewinds the workspace to it, running processes included, and a
// copy can start from any step. Each save records the save it came from, so
// the steps form a tree: rewinding then saving again starts a branch.

// stepSave full-saves a workspace and resumes it at once: one step recorded,
// the agent carries on.
func stepSave(e *Env, ws string) *client.Save {
	e.T.Helper()
	sv, err := e.C.Save(e.Ctx, ws, client.FullSave)
	e.Must(err)
	e.Resume(sv.ID)
	return sv
}

func saveParent(e *Env, id string) string {
	e.T.Helper()
	sv, err := e.C.GetSave(e.Ctx, id)
	e.Must(err)
	return sv.Parent
}

func baseSave(e *Env, ws string) string {
	e.T.Helper()
	w, err := e.C.GetWorkspace(e.Ctx, ws)
	e.Must(err)
	return w.BaseSave
}

// sleeper starts a process that lives until killed and returns its pid.
func sleeper(e *Env, ws, name string) string {
	e.T.Helper()
	background(e, ws, "echo $$ > /tmp/"+name+".pid; exec sleep 100000")
	for i := 0; i < 50; i++ {
		if b, err := e.C.ReadFile(e.Ctx, ws, "tmp/"+name+".pid"); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
		e.Sh(ws, "sleep 0.1")
	}
	e.T.Fatalf("%s did not start", name)
	return ""
}

// alive: the sleeper with this pid is running. The pid alone is not enough:
// rewinding also rewinds the pid counter, so a later process may reuse it.
func alive(e *Env, ws, pid string) bool {
	e.T.Helper()
	res, err := e.C.Exec(e.Ctx, ws, client.Exec{Command: []string{"sh", "-c", `[ "$(tr '\0' ' ' < /proc/` + pid + `/cmdline)" = "sleep 100000 " ]`}})
	e.Must(err)
	return res.ExitCode == 0
}

func init() {
	register("G18.2", func(e *Env) {
		ws := e.Workspace("history")
		Equal(e, "a new workspace's base", baseSave(e, ws.ID), "")
		e.Write(ws.ID, "step", "1")
		a := e.Save(ws.ID)
		e.Write(ws.ID, "step", "2")
		b := e.Save(ws.ID)
		Equal(e, "the first save's parent", saveParent(e, a.ID), "")
		Equal(e, "each save's parent is the one before", saveParent(e, b.ID), a.ID)
		Equal(e, "the workspace's base after saving", baseSave(e, ws.ID), b.ID)

		// rewind to a, then save: a branch off a, beside b
		e.Resume(a.ID)
		Equal(e, "the workspace's base after rewinding", baseSave(e, ws.ID), a.ID)
		e.Write(ws.ID, "step", "2-other")
		c := e.Save(ws.ID)
		Equal(e, "a save after rewinding branches from the step rewound to", saveParent(e, c.ID), a.ID)

		// a copy's history goes on from the save it was copied from
		copies, err := e.C.Copy(e.Ctx, b.ID, 1)
		e.Must(err)
		Equal(e, "a copy's base", baseSave(e, copies[0].ID), b.ID)
		d := e.Save(copies[0].ID)
		Equal(e, "a save in a copy continues from the copied save", saveParent(e, d.ID), b.ID)

		// the tree is still there after the engine restarts
		if e.Engine != nil {
			e.Restart()
			Equal(e, "parent after a restart", saveParent(e, c.ID), a.ID)
			Equal(e, "base after a restart", baseSave(e, ws.ID), c.ID)
		}
	})

	register("G18.3", func(e *Env) {
		ws := e.Workspace("rewind")
		e.Write(ws.ID, "root/notes.txt", "step 1\n")
		first := sleeper(e, ws.ID, "first")
		s1 := stepSave(e, ws.ID)

		e.Write(ws.ID, "root/notes.txt", "step 1\nstep 2\n")
		second := sleeper(e, ws.ID, "second")
		s2 := stepSave(e, ws.ID)

		// step 3 goes wrong: files deleted, processes killed
		e.Sh(ws.ID, "rm -rf /root/notes.txt; kill "+first+" "+second+"; true")

		e.Resume(s2.ID)
		Equal(e, "files at step 2", e.Read(ws.ID, "root/notes.txt"), "step 1\nstep 2\n")
		if !alive(e, ws.ID, first) || !alive(e, ws.ID, second) {
			e.T.Fatal("rewound to step 2: the processes running then are not back")
		}

		e.Resume(s1.ID)
		Equal(e, "files at step 1", e.Read(ws.ID, "root/notes.txt"), "step 1\n")
		if !alive(e, ws.ID, first) {
			e.T.Fatal("rewound to step 1: its process is not back")
		}
		if alive(e, ws.ID, second) {
			e.T.Fatal("rewound to step 1: a process started at step 2 is still running")
		}
	})

	register("G18.6", func(e *Env) {
		ws := e.Workspace("branches")
		var steps []*client.Save
		for _, s := range []string{"1", "2", "3"} {
			e.Write(ws.ID, "step", s)
			steps = append(steps, e.Save(ws.ID))
		}
		e.Write(ws.ID, "step", "4") // the agent has moved on
		for i, sv := range steps {
			copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
			e.Must(err)
			Equal(e, "a copy of step "+string(rune('1'+i)), e.Read(copies[0].ID, "step"), string(rune('1'+i)))
		}
		Equal(e, "the workspace itself is untouched", e.Read(ws.ID, "step"), "4")
	})
}
