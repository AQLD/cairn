package acceptance

import (
	"time"

	"github.com/AQLD/cairn/internal/failpoint"
)

// wsState fails unless the workspace is in the given state.
func wsState(e *Env, id, want string) {
	e.T.Helper()
	w, err := e.C.GetWorkspace(e.Ctx, id)
	e.Must(err)
	Equal(e, "workspace state", w.State, want)
}

// noAutoSaves fails if a pause's save outlived the pause.
func noAutoSaves(e *Env, id string) {
	e.T.Helper()
	saves, err := e.C.ListSaves(e.Ctx, id)
	e.Must(err)
	for _, sv := range saves {
		if sv.Auto {
			e.T.Fatalf("pause save %s still there after waking", sv.ID)
		}
	}
}

func init() {
	// G4.4: unused workspaces are paused; using them wakes them as they were.
	register("G4.4", func(e *Env) {
		ws := e.Workspace("idle")
		background(e, ws.ID, counterProgram)
		before := running(e, ws.ID)

		// not idle yet: nothing happens
		n, err := e.Engine.PauseIdle(e.Ctx, time.Hour)
		e.Must(err)
		Equal(e, "paused while in use", n, 0)

		e.Clock.Advance(2 * time.Hour)
		n, err = e.Engine.PauseIdle(e.Ctx, time.Hour)
		e.Must(err)
		Equal(e, "paused when idle", n, 1)
		wsState(e, ws.ID, "paused")

		// the pause's save is the engine's: listed, but not for resuming or deleting
		saves, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		var auto string
		for _, sv := range saves {
			if sv.Auto {
				auto = sv.ID
			}
		}
		if auto == "" {
			e.T.Fatal("no pause save listed")
		}
		_, err = e.C.Resume(e.Ctx, auto)
		e.ErrCode(err, "conflict")
		e.ErrCode(e.C.DeleteSave(e.Ctx, auto), "conflict")

		// reading files does not wake it; a command does
		_, err = e.C.ReadFile(e.Ctx, ws.ID, "tmp/counter.json")
		e.Must(err)
		wsState(e, ws.ID, "paused")
		e.Sh(ws.ID, "true")
		wsState(e, ws.ID, "running")
		after := running(e, ws.ID)
		Equal(e, "same process after waking", after.PID, before.PID)
		noAutoSaves(e, ws.ID)

		// an explicit pause; a write wakes it
		_, err = e.C.PauseWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		wsState(e, ws.ID, "paused")
		e.Write(ws.ID, "note.txt", "hello")
		wsState(e, ws.ID, "running")
		Equal(e, "process after write-wake", running(e, ws.ID).PID, before.PID)
	})

	// G4.5: shutdown pauses everything; after a restart each workspace wakes
	// with its processes.
	register("G4.5", func(e *Env) {
		a, b := e.Workspace("a"), e.Workspace("b")
		background(e, a.ID, counterProgram)
		background(e, b.ID, counterProgram)
		pa, pb := running(e, a.ID), running(e, b.ID)

		paused, failed := e.Engine.PauseAll(e.Ctx)
		Equal(e, "paused", paused, 2)
		Equal(e, "failed", failed, 0)
		e.Restart() // sandboxes and engine gone, as when a pod moves

		wsState(e, a.ID, "paused")
		e.Sh(a.ID, "true")
		e.Sh(b.ID, "true")
		Equal(e, "a's process", running(e, a.ID).PID, pa.PID)
		Equal(e, "b's process", running(e, b.ID).PID, pb.PID)
	})

	// G4.6: crashes in the middle of pausing and of waking.
	register("G4.6", func(e *Env) {
		ws := e.Workspace("crashy")
		background(e, ws.ID, counterProgram)
		before := running(e, ws.ID)

		e.Failpoints.Enable(failpoint.PauseAfterCommit, failpoint.ErrCrash)
		_, err := e.C.PauseWorkspace(e.Ctx, ws.ID)
		if err == nil {
			e.T.Fatal("pause should have crashed")
		}
		e.Restart()
		wsState(e, ws.ID, "paused")

		e.Failpoints.Enable(failpoint.WakeBeforeRestore, failpoint.ErrCrash)
		if _, err := e.C.Run(e.Ctx, ws.ID, "true"); err == nil {
			e.T.Fatal("wake should have crashed")
		}
		e.Restart()
		wsState(e, ws.ID, "paused")

		e.Sh(ws.ID, "true")
		Equal(e, "same process after both crashes", running(e, ws.ID).PID, before.PID)
		noAutoSaves(e, ws.ID)
	})
}
