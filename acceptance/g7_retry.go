package acceptance

import (
	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/failpoint"
)

// G7.9: retried requests never do the work twice.

func init() {
	register("G7.9.1", func(e *Env) {
		ctx := client.WithIdempotencyKey(e.Ctx, "create-1")
		a, err := e.C.CreateWorkspace(ctx, client.CreateWorkspace{Name: "retry"})
		e.Must(err)
		b, err := e.C.CreateWorkspace(ctx, client.CreateWorkspace{Name: "retry"})
		e.Must(err)
		Equal(e, "same workspace", b.ID, a.ID)
		list, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces", len(list), 1)
		// without a key, a second create is a new workspace
		_, err = e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "retry"})
		e.Must(err)
		list, err = e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces", len(list), 2)
	})

	register("G7.9.2", func(e *Env) {
		ws := e.Workspace("retry")
		ctx := client.WithIdempotencyKey(e.Ctx, "save-1")
		a, err := e.C.Save(ctx, ws.ID, client.QuickSave)
		e.Must(err)
		e.Write(ws.ID, "after", "written after the first save")
		b, err := e.C.Save(ctx, ws.ID, client.QuickSave)
		e.Must(err)
		Equal(e, "same save", b.ID, a.ID)
		saves, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "saves", len(saves), 1)
	})

	register("G7.9.3", func(e *Env) {
		ws := e.Workspace("retry")
		sv := e.Save(ws.ID)
		ctx := client.WithIdempotencyKey(e.Ctx, "copy-1")
		a, err := e.C.Copy(ctx, sv.ID, 3)
		e.Must(err)
		b, err := e.C.Copy(ctx, sv.ID, 3)
		e.Must(err)
		Equal(e, "copies", len(b), 3)
		for i := range a {
			Equal(e, "same copy", b[i].ID, a[i].ID)
		}
		list, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces", len(list), 4)
	})

	register("G7.9.4", func(e *Env) {
		ws := e.Workspace("retry")
		e.Write(ws.ID, "f", "saved")
		ctx := client.WithIdempotencyKey(e.Ctx, "save-crash")
		e.Failpoints.Enable(failpoint.SaveAfterCommit, failpoint.ErrCrash)
		if _, err := e.C.Save(ctx, ws.ID, client.QuickSave); err == nil {
			e.T.Fatal("save reported success despite the crash")
		}
		e.Restart()
		ctx = client.WithIdempotencyKey(e.Ctx, "save-crash")
		sv, err := e.C.Save(ctx, ws.ID, client.QuickSave)
		e.Must(err)
		saves, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "saves after retry", len(saves), 1)
		Equal(e, "retry returned the save that reached disk", sv.ID, saves[0].ID)
	})

	register("G7.9.5", func(e *Env) {
		ws := e.Workspace("retry")
		sv := e.Save(ws.ID)
		ctx := client.WithIdempotencyKey(e.Ctx, "copy-crash")
		e.Failpoints.Enable(failpoint.CopyBeforeRunning, failpoint.ErrCrash)
		if _, err := e.C.Copy(ctx, sv.ID, 1); err == nil {
			e.T.Fatal("copy reported success despite the crash")
		}
		e.Restart()
		ctx = client.WithIdempotencyKey(e.Ctx, "copy-crash")
		copies, err := e.C.Copy(ctx, sv.ID, 1)
		e.Must(err)
		again, err := e.C.Copy(ctx, sv.ID, 1)
		e.Must(err)
		Equal(e, "retry is stable", again[0].ID, copies[0].ID)
		list, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "original + one copy", len(list), 2)
	})

	register("G7.9.6", func(e *Env) {
		a := e.Workspace("a")
		b := e.Workspace("b")
		ctx := client.WithIdempotencyKey(e.Ctx, "shared-key")
		_, err := e.C.Save(ctx, a.ID, client.QuickSave)
		e.Must(err)
		_, err = e.C.Save(ctx, b.ID, client.QuickSave)
		e.ErrCode(err, "conflict")
		sv := e.Save(a.ID)
		_, err = e.C.Copy(client.WithIdempotencyKey(e.Ctx, "copy-key"), sv.ID, 1)
		e.Must(err)
		other := e.Save(b.ID)
		_, err = e.C.Copy(client.WithIdempotencyKey(e.Ctx, "copy-key"), other.ID, 1)
		e.ErrCode(err, "conflict")
		_, err = e.C.CreateWorkspace(client.WithIdempotencyKey(e.Ctx, "bad#key"), client.CreateWorkspace{})
		e.ErrCode(err, "invalid_argument")
	})
}
