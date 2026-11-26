package acceptance

import (
	"strings"

	"github.com/AQLD/cairn/client"
)

// lostServer makes a workspace with a file (and, where full saves work, a
// running process), saves it to the bucket, then replaces the server with a
// new one on an empty disk and the same bucket.
func lostServer(e *Env) (ws *client.Workspace, sv *client.Save, pid int) {
	e.T.Helper()
	withObjectStore(e)
	ws = e.Workspace("on-the-lost-server")
	e.Write(ws.ID, "work.txt", "made on the first server")
	kind := client.QuickSave
	if e.Info.Capabilities.FullSave {
		background(e, ws.ID, counterProgram)
		pid = running(e, ws.ID).PID
		kind = client.FullSave
	}
	sv, err := e.C.Save(e.Ctx, ws.ID, kind)
	e.Must(err)
	e.Must(e.Engine.SyncRemote(e.Ctx))
	_ = e.Engine.StopAll(e.Ctx)
	e.stop()
	lost := e.DataDir
	e.T.Cleanup(func() { unmountAll(lost) })
	e.DataDir = e.T.TempDir()
	e.start()
	return ws, sv, pid
}

func init() {
	register("G7.17.1", func(e *Env) {
		ws, sv, pid := lostServer(e)
		got, err := e.C.ResumeWith(e.Ctx, sv.ID, client.ResumeOptions{Adopt: true})
		e.Must(err)
		Equal(e, "adopted workspace's ID", got.ID, ws.ID)
		Equal(e, "adopted workspace's name", got.Name, ws.Name)
		Equal(e, "state", got.State, "running")
		Equal(e, "files carried over", e.Read(ws.ID, "work.txt"), "made on the first server")
		if pid != 0 {
			Equal(e, "the process carries on under the same workspace", running(e, ws.ID).PID, pid)
		}
		list, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		if len(list) != 1 || list[0].ID != ws.ID {
			e.T.Fatalf("workspaces after adopting: %+v, want just %s", list, ws.ID)
		}
		// it is an ordinary workspace here now: saves and resumes as usual
		e.Write(ws.ID, "after.txt", "here")
		again := e.Save(ws.ID)
		_, err = e.C.Resume(e.Ctx, again.ID)
		e.Must(err)
		Equal(e, "after a plain resume here", e.Read(ws.ID, "after.txt"), "here")
	})

	register("G7.17.2", func(e *Env) {
		_, sv, _ := lostServer(e)
		_, err := e.C.Resume(e.Ctx, sv.ID)
		e.ErrCode(err, "conflict")
		if msg := err.Error(); !strings.Contains(msg, "adopt") || !strings.Contains(msg, "copy") {
			e.T.Fatalf("refusal %q: want it to name adopt and copy", msg)
		}
		list, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces after a refused resume", len(list), 0)
	})

	register("G7.17.3", func(e *Env) {
		ws := e.Workspace("deleted-here")
		e.Write(ws.ID, "work.txt", "x")
		sv := e.Save(ws.ID)
		e.Must(e.C.DeleteWorkspace(e.Ctx, ws.ID))
		_, err := e.C.ResumeWith(e.Ctx, sv.ID, client.ResumeOptions{Adopt: true})
		e.ErrCode(err, "conflict")
		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		Equal(e, "a copy still works", e.Read(copies[0].ID, "work.txt"), "x")
	})
}
