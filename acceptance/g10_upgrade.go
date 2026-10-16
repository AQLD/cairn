package acceptance

import (
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/AQLD/cairn/client"
)

// G10.6: the current engine takes over a data folder written by the previous
// one (CAIRN_PREVIOUS_CAIRND, with its cairn-init in CAIRN_PREVIOUS_INIT):
// workspaces, their files and processes, quick and full saves, and paused
// workspaces all carry over. CI builds the previous engine from the branch
// the change merges into.

func init() {
	register("G10.6", func(e *Env) {
		oldBin, oldInit := os.Getenv("CAIRN_PREVIOUS_CAIRND"), os.Getenv("CAIRN_PREVIOUS_INIT")
		if oldBin == "" {
			e.T.Skip("needs CAIRN_PREVIOUS_CAIRND: the previous engine's cairnd (and CAIRN_PREVIOUS_INIT, its cairn-init)")
		}
		if oldInit == "" {
			oldInit = testInit(e.T)
		}
		full := e.Info.Capabilities.FullSave
		data := e.T.TempDir()
		e.T.Cleanup(func() { unmountAll(data) })
		own := e.C
		defer func() { e.C = own }()

		// --- the previous engine writes the data folder ---
		proc, c := startCairndWith(e, oldBin, oldInit, data)
		e.C = c
		files := e.Workspace("files")
		e.Write(files.ID, "notes/todo.txt", "ship it")
		quick := e.Save(files.ID)
		e.Write(files.ID, "notes/later.txt", "after the save")

		var saved, paused *client.Workspace
		var fullSave *client.Save
		var savedPID, pausedPID int
		if full {
			saved = e.Workspace("full-saved")
			background(e, saved.ID, counterProgram)
			savedPID = running(e, saved.ID).PID
			sv, err := e.C.Save(e.Ctx, saved.ID, client.FullSave)
			e.Must(err)
			fullSave = sv

			paused = e.Workspace("paused")
			background(e, paused.ID, counterProgram)
			pausedPID = running(e, paused.ID).PID
			_, err = e.C.PauseWorkspace(e.Ctx, paused.ID)
			e.Must(err)
		}
		stopCairnd(e, proc)

		// --- the upgrade: the current engine on the same folder ---
		proc, c = startCairnd(e, buildCairnd(e), data)
		defer func() {
			ws, _ := c.ListWorkspaces(e.Ctx)
			for _, w := range ws {
				_ = c.DeleteWorkspace(e.Ctx, w.ID)
			}
			_ = proc.Process.Kill()
			_ = proc.Wait()
		}()
		e.C = c

		Equal(e, "files", e.Read(files.ID, "notes/todo.txt"), "ship it")
		Equal(e, "files written after the save", e.Read(files.ID, "notes/later.txt"), "after the save")
		Equal(e, "commands run", e.Sh(files.ID, "echo ok"), "ok\n")
		_, err := e.C.VerifySave(e.Ctx, quick.ID)
		e.Must(err)
		e.Resume(quick.ID)
		if _, err := e.C.ReadFile(e.Ctx, files.ID, "notes/later.txt"); err == nil {
			e.T.Fatal("resuming the quick save kept a file written after it")
		}
		copies, err := e.C.Copy(e.Ctx, quick.ID, 1)
		e.Must(err)
		Equal(e, "copy of an old save", e.Read(copies[0].ID, "notes/todo.txt"), "ship it")
		e.Save(files.ID) // and the new engine saves old workspaces

		if full {
			e.Resume(fullSave.ID)
			Equal(e, "process in an old full save", running(e, saved.ID).PID, savedPID)
			// a paused workspace wakes on its first command, processes intact
			Equal(e, "paused workspace wakes", e.Sh(paused.ID, "echo awake"), "awake\n")
			Equal(e, "process of the paused workspace", running(e, paused.ID).PID, pausedPID)
			sv, err := e.C.Save(e.Ctx, saved.ID, client.FullSave)
			e.Must(err)
			e.Resume(sv.ID)
			Equal(e, "full save by the new engine", running(e, saved.ID).PID, savedPID)
		}
	})
}

// stopCairnd stops cairnd the way a service manager does: SIGTERM, then
// SIGKILL if it has not exited in time. Sandboxes keep running (the harness
// starts cairnd with --pause-on-shutdown=false) and the next engine adopts them.
func stopCairnd(e *Env, proc *exec.Cmd) {
	e.T.Helper()
	_ = proc.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		_ = proc.Process.Kill()
		<-done
	}
}
