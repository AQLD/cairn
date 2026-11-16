package acceptance

import (
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/failpoint"
)

// keepRunningSave full-saves a workspace and asks it to carry on.
func keepRunningSave(e *Env, ws string) (*client.Save, time.Duration) {
	e.T.Helper()
	start := time.Now()
	sv, err := e.C.SaveWith(e.Ctx, ws, client.SaveOptions{Kind: client.FullSave, KeepRunning: true})
	e.Must(err)
	return sv, time.Since(start)
}

func init() {
	register("G18.7.1", func(e *Env) {
		ws := e.Workspace("keep-running")
		background(e, ws.ID, counterProgram)
		before := running(e, ws.ID)
		s := session(e, ws.ID, "python")
		cell(e, ws.ID, s.ID, "answer = 41")

		sv, took := keepRunningSave(e, ws.ID)
		e.T.Logf("full save that keeps running took %s", took.Round(time.Millisecond))
		Equal(e, "save kind", sv.Kind, client.FullSave)
		Equal(e, "save state", sv.State, "ready")

		// carries on at once: no resume, the same process still counting.
		// Checked through file reads first: a command would let the runtime
		// heal a sandbox left frozen, hiding a save that never thawed it.
		after := running(e, ws.ID)
		Equal(e, "pid after the save", after.PID, before.PID)
		Equal(e, "the counter's start time (same process, not a restart)", after.Start, before.Start)
		got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "state after the save", got.State, "running")
		Equal(e, "the workspace's base", got.BaseSave, sv.ID)
		Equal(e, "a command right after the save", e.Sh(ws.ID, "echo ok"), "ok\n")
		Equal(e, "session variable after the save", strings.TrimSpace(cell(e, ws.ID, s.ID, "answer + 1")), "42")

		// a real save: resuming it later rewinds to the saved moment
		time.Sleep(time.Second)
		later := running(e, ws.ID)
		e.Write(ws.ID, "after-save.txt", "written after the save")
		e.Resume(sv.ID)
		back := readCounter(e, ws.ID)
		if back.PID != before.PID || back.N >= later.N {
			e.T.Fatalf("after resuming the save: pid %d n %d, want pid %d and n below %d (the saved moment)", back.PID, back.N, before.PID, later.N)
		}
		running(e, ws.ID)
		if _, err := e.C.ReadFile(e.Ctx, ws.ID, "after-save.txt"); err == nil {
			e.T.Fatal("a file written after the save survived resuming it")
		}
		Equal(e, "session variable after rewinding", strings.TrimSpace(cell(e, ws.ID, s.ID, "answer")), "41")
	})

	register("G18.7.2", func(e *Env) {
		for _, point := range []string{failpoint.SaveBeforeThaw, failpoint.SaveAfterThaw} {
			ws := e.Workspace("keep-running-crash")
			background(e, ws.ID, counterProgram)
			before := running(e, ws.ID)

			e.Failpoints.Enable(point, failpoint.ErrCrash)
			if _, err := e.C.SaveWith(e.Ctx, ws.ID, client.SaveOptions{Kind: client.FullSave, KeepRunning: true}); err == nil {
				e.T.Fatalf("%s: save reported success despite the crash", point)
			}
			e.Failpoints.Disable(point)
			e.Restart()

			saves, err := e.C.ListSaves(e.Ctx, ws.ID)
			e.Must(err)
			if len(saves) != 1 || saves[0].State != "ready" || saves[0].Kind != client.FullSave {
				e.T.Fatalf("%s: saves after the crash %+v, want one ready full save", point, saves)
			}
			got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
			e.Must(err)
			Equal(e, point+": state after the crash", got.State, "running")
			// the same process carries on: never restarted from its files
			after := running(e, ws.ID)
			Equal(e, point+": pid after the crash", after.PID, before.PID)
			Equal(e, point+": counter start after the crash", after.Start, before.Start)

			// and the save it was making is a real one
			e.Resume(saves[0].ID)
			back := running(e, ws.ID)
			Equal(e, point+": pid after resuming the save", back.PID, before.PID)
			Equal(e, point+": counter start after resuming the save", back.Start, before.Start)
		}
	})

	register("G18.7.3", func(e *Env) {
		ws := e.Workspace("keep-running-honest")
		e.Write(ws.ID, "f", "quick")
		sv, err := e.C.SaveWith(e.Ctx, ws.ID, client.SaveOptions{Kind: client.QuickSave, KeepRunning: true})
		e.Must(err)
		Equal(e, "quick save kind", sv.Kind, client.QuickSave)
		got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "state after a quick save", got.State, "running")
		Equal(e, "a command after a quick save", e.Sh(ws.ID, "cat f"), "quick")

		if e.Info.Capabilities.FullSave {
			return
		}
		_, err = e.C.SaveWith(e.Ctx, ws.ID, client.SaveOptions{Kind: client.FullSave, KeepRunning: true})
		e.ErrCode(err, "unsupported_by_runtime")
		got, err = e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "state after a refused full save", got.State, "running")
	})
}
