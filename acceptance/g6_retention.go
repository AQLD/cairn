package acceptance

import (
	"os"
	"time"

	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/failpoint"
)

// G6.8: deleted saves can be recovered within the retention window.

func init() {
	register("G6.8.1", func(e *Env) {
		ws := e.Workspace("hide")
		e.Write(ws.ID, "f", "saved")
		sv := e.Save(ws.ID)
		e.Must(e.C.DeleteSave(e.Ctx, sv.ID))

		list, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "listed saves", len(list), 0)

		got, err := e.C.GetSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "state", got.State, "deleted")
		if got.DeletedAt == nil || got.PurgeAfter == nil || !got.PurgeAfter.After(*got.DeletedAt) {
			e.T.Fatalf("deleted save must say when it will be purged: %+v", got)
		}

		_, err = e.C.Resume(e.Ctx, sv.ID)
		e.ErrCode(err, "conflict")
		_, err = e.C.Copy(e.Ctx, sv.ID, 1)
		e.ErrCode(err, "conflict")
	})

	register("G6.8.2", func(e *Env) {
		ws := e.Workspace("undelete")
		e.Write(ws.ID, "f", "saved")
		sv := e.Save(ws.ID)
		e.Must(e.C.DeleteSave(e.Ctx, sv.ID))
		got, err := e.C.UndeleteSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "state", got.State, "ready")
		if got.DeletedAt != nil {
			e.T.Fatal("undeleted save still has a deletion time")
		}
		list, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "listed again", len(list), 1)

		e.Write(ws.ID, "f", "changed")
		e.Resume(sv.ID)
		Equal(e, "content intact", e.Read(ws.ID, "f"), "saved")
	})

	register("G6.8.3", func(e *Env) {
		ws := e.Workspace("purge")
		sv := e.Save(ws.ID)
		keep := e.Save(ws.ID)
		e.Must(e.C.DeleteSave(e.Ctx, sv.ID))

		// one day before the window ends: nothing is purged
		e.Clock.Advance(engine.DefaultRetention - 24*time.Hour)
		n, err := e.Engine.PurgeExpired(e.Ctx)
		e.Must(err)
		Equal(e, "purged before the window ended", n, 0)

		e.Clock.Advance(48 * time.Hour)
		n, err = e.Engine.PurgeExpired(e.Ctx)
		e.Must(err)
		Equal(e, "purged", n, 1)

		_, err = e.C.GetSave(e.Ctx, sv.ID)
		e.ErrCode(err, "not_found")
		_, err = e.C.UndeleteSave(e.Ctx, sv.ID)
		e.ErrCode(err, "not_found")
		if _, err := os.Stat(e.Engine.SaveDir(sv.ID)); !os.IsNotExist(err) {
			e.T.Fatal("purged save's folder is still on disk")
		}
		// saves that were not deleted are untouched
		e.Resume(keep.ID)
	})

	register("G6.8.4", func(e *Env) {
		ws := e.Workspace("retry")
		sv := e.Save(ws.ID)
		e.Must(e.C.DeleteSave(e.Ctx, sv.ID))
		e.Must(e.C.DeleteSave(e.Ctx, sv.ID)) // again: no error, still deleted
		first, err := e.C.GetSave(e.Ctx, sv.ID)
		e.Must(err)
		_, err = e.C.UndeleteSave(e.Ctx, sv.ID)
		e.Must(err)
		again, err := e.C.UndeleteSave(e.Ctx, sv.ID) // again: no error, still ready
		e.Must(err)
		Equal(e, "state", again.State, "ready")
		if first.DeletedAt == nil {
			e.T.Fatal("second delete cleared the deletion time")
		}
		e.ErrCode(e.C.DeleteSave(e.Ctx, "sv_doesnotexist"), "not_found")
	})

	register("G6.8.5", func(e *Env) {
		ws := e.Workspace("purge-crash")
		sv := e.Save(ws.ID)
		e.Must(e.C.DeleteSave(e.Ctx, sv.ID))
		e.Clock.Advance(engine.DefaultRetention + time.Hour)
		e.Failpoints.Enable(failpoint.PurgeAfterRemove, failpoint.ErrCrash)
		if _, err := e.Engine.PurgeExpired(e.Ctx); err == nil {
			e.T.Fatal("purge reported success despite the crash")
		}
		// the folder is gone but the record remains; reopening the engine (same
		// clock) purges again and finishes the job
		e.Restart()
		_, err := e.C.GetSave(e.Ctx, sv.ID)
		e.ErrCode(err, "not_found")
	})
}
