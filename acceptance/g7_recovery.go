package acceptance

import (
	"os"
	"path/filepath"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/failpoint"
)

// G7: the engine survives crashes and lost servers.

func init() {
	register("G7.1", func(e *Env) {
		a := e.Workspace("a")
		b := e.Workspace("b")
		e.Write(a.ID, "f", "A")
		e.Write(b.ID, "f", "B")
		sv := e.Save(a.ID)
		e.Restart()

		list, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces after restart", len(list), 2)
		Equal(e, "a", e.Read(a.ID, "f"), "A")
		Equal(e, "b", e.Read(b.ID, "f"), "B")
		got, err := e.C.GetSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "save after restart", got.State, "ready")
		e.Resume(sv.ID)
	})

	register("G7.2", func(e *Env) {
		ws := e.Workspace("committed")
		e.Write(ws.ID, "f", "on disk")
		e.Failpoints.Enable(failpoint.SaveAfterCommit, failpoint.ErrCrash)
		_, err := e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		if err == nil {
			e.T.Fatal("save reported success despite the crash")
		}
		e.Restart()
		saves, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "published saves", len(saves), 1)
		e.Write(ws.ID, "f", "changed")
		e.Resume(saves[0].ID)
		Equal(e, "restored", e.Read(ws.ID, "f"), "on disk")
	})

	register("G7.3.1", func(e *Env) {
		ws := e.Workspace("resume-crash")
		e.Write(ws.ID, "f", "saved")
		sv := e.Save(ws.ID)
		e.Write(ws.ID, "f", "current")
		e.Failpoints.Enable(failpoint.ResumeBeforeSwap, failpoint.ErrCrash)
		if _, err := e.C.Resume(e.Ctx, sv.ID); err == nil {
			e.T.Fatal("resume reported success despite the crash")
		}
		e.Restart()
		got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "state", got.State, "running")
		Equal(e, "rolled back to the current filesystem", e.Read(ws.ID, "f"), "current")
		assertNoSwapLeftovers(e, ws.ID)
		e.Resume(sv.ID)
		Equal(e, "resume works afterwards", e.Read(ws.ID, "f"), "saved")
	})

	register("G7.3.2", func(e *Env) {
		ws := e.Workspace("resume-crash")
		e.Write(ws.ID, "f", "saved")
		sv := e.Save(ws.ID)
		e.Write(ws.ID, "f", "current")
		e.Failpoints.Enable(failpoint.ResumeAfterSwap, failpoint.ErrCrash)
		if _, err := e.C.Resume(e.Ctx, sv.ID); err == nil {
			e.T.Fatal("resume reported success despite the crash")
		}
		e.Restart()
		got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "state", got.State, "running")
		Equal(e, "completed to the saved filesystem", e.Read(ws.ID, "f"), "saved")
		assertNoSwapLeftovers(e, ws.ID)
	})

	register("G7.4", func(e *Env) {
		e.Failpoints.Enable(failpoint.CreateBeforeReady, failpoint.ErrCrash)
		if _, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "half"}); err == nil {
			e.T.Fatal("create reported success despite the crash")
		}
		e.Restart()
		list, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces", len(list), 1)
		Equal(e, "state", list[0].State, "running")
		e.Write(list[0].ID, "f", "usable")
		Equal(e, "usable", e.Read(list[0].ID, "f"), "usable")
	})

	register("G7.5", func(e *Env) {
		ws := e.Workspace("source")
		sv := e.Save(ws.ID)
		e.Failpoints.Enable(failpoint.CopyBeforeRunning, failpoint.ErrCrash)
		if _, err := e.C.Copy(e.Ctx, sv.ID, 1); err == nil {
			e.T.Fatal("copy reported success despite the crash")
		}
		e.Restart()
		list, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "only the original remains", len(list), 1)
		Equal(e, "original", list[0].ID, ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		Equal(e, "copy works afterwards", len(copies), 1)
	})
}

func assertNoSwapLeftovers(e *Env, id string) {
	e.T.Helper()
	for _, name := range []string{"rootfs.next", "rootfs.prev"} {
		if _, err := os.Stat(filepath.Join(e.DataDir, "workspaces", id, name)); err == nil {
			e.T.Fatalf("%s left behind", name)
		}
	}
}
