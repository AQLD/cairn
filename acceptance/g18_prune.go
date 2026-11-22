package acceptance

import (
	"fmt"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
)

// G18.5: old steps are pruned by policy while pinned steps are kept.

func init() {
	register("G18.5.1", func(e *Env) {
		ws := e.Workspace("pin")
		e.Write(ws.ID, "step", "1")
		sv := e.Save(ws.ID)
		if sv.Pinned {
			e.T.Fatal("a new save is pinned")
		}

		pinned, err := e.C.PinSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "pinned after pin", pinned.Pinned, true)
		again, err := e.C.PinSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "pinning twice", again.Pinned, true)

		saves, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "saves listed", len(saves), 1)
		Equal(e, "pinned in the workspace's list", saves[0].Pinned, true)

		e.ErrCode(e.C.DeleteSave(e.Ctx, sv.ID), "conflict")

		e.Restart()
		got, err := e.C.GetSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "pinned after a restart", got.Pinned, true)
		Equal(e, "still ready", got.State, "ready")

		unpinned, err := e.C.UnpinSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "pinned after unpin", unpinned.Pinned, false)
		e.Must(e.C.DeleteSave(e.Ctx, sv.ID))

		_, err = e.C.PinSave(e.Ctx, sv.ID)
		e.ErrCode(err, "conflict") // deleted: undelete first
		_, err = e.C.PinSave(e.Ctx, "sv_doesnotexist")
		e.ErrCode(err, "not_found")
	})
}

// saves makes n quick saves of ws, oldest first.
func saves(e *Env, ws string, n int) []*client.Save {
	e.T.Helper()
	out := make([]*client.Save, n)
	for i := range out {
		e.Write(ws, "step", fmt.Sprint(i))
		out[i] = e.Save(ws)
	}
	return out
}

// prune runs the engine's pruning now and returns how many saves it deleted.
func prune(e *Env) int {
	e.T.Helper()
	n, err := e.Engine.Prune(e.Ctx)
	e.Must(err)
	return n
}

// stateOf is a save's state ("ready", "deleted"...).
func stateOf(e *Env, id string) string {
	e.T.Helper()
	sv, err := e.C.GetSave(e.Ctx, id)
	e.Must(err)
	return sv.State
}

func init() {
	register("G18.5.2", func(e *Env) {
		ws := e.Workspace("prune")
		old := saves(e, ws.ID, 25)

		e.Clock.Advance(6 * 24 * time.Hour)
		Equal(e, "pruned while every save is under 7 days old", prune(e), 0)

		e.Clock.Advance(2 * 24 * time.Hour)
		recent := saves(e, ws.ID, 3)
		Equal(e, "pruned", prune(e), 8) // newest 20 = 3 recent + 17 old
		for i, sv := range old {
			want := "ready"
			if i < 8 {
				want = "deleted"
			}
			Equal(e, fmt.Sprintf("old save %d", i), stateOf(e, sv.ID), want)
		}
		for _, sv := range recent {
			Equal(e, "recent save", stateOf(e, sv.ID), "ready")
		}
		list, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "saves listed after pruning", len(list), 20)
		Equal(e, "pruning again", prune(e), 0)

		back, err := e.C.UndeleteSave(e.Ctx, old[0].ID)
		e.Must(err)
		Equal(e, "a pruned save can be undeleted", back.State, "ready")
		e.Resume(old[0].ID)
		Equal(e, "the undeleted save restores", e.Read(ws.ID, "step"), "0")
	})

	register("G18.5.3", func(e *Env) {
		a := e.Workspace("prune-keep")
		as := saves(e, a.ID, 22)
		_, err := e.C.PinSave(e.Ctx, as[0].ID)
		e.Must(err)
		copies, err := e.C.Copy(e.Ctx, as[1].ID, 1) // the copy stands on as[1]
		e.Must(err)

		e.Clock.Advance(8 * 24 * time.Hour)
		Equal(e, "pruned while the two old saves are protected", prune(e), 0)
		e.Write(a.ID, "step", "next")
		e.Save(a.ID)
		Equal(e, "pruned after one more save", prune(e), 1)
		Equal(e, "pinned old save", stateOf(e, as[0].ID), "ready")
		Equal(e, "old save a copy stands on", stateOf(e, as[1].ID), "ready")
		Equal(e, "oldest unprotected save", stateOf(e, as[2].ID), "deleted")
		Equal(e, "copy is intact", e.Read(copies[0].ID, "step"), "1")
	})

	register("G18.5.5", func(e *Env) {
		b := e.Workspace("prune-paused")
		bs := saves(e, b.ID, 21)
		_, err := e.C.PauseWorkspace(e.Ctx, b.ID)
		e.Must(err)
		e.Clock.Advance(8 * 24 * time.Hour)
		Equal(e, "pruned", prune(e), 1)
		Equal(e, "oldest save of the paused workspace", stateOf(e, bs[0].ID), "deleted")
		Equal(e, "paused workspace wakes as it was", e.Read(b.ID, "step"), "20")
	})

	register("G18.5.4", func(e *Env) {
		e.RestartWith(func(c *engine.Config) { c.KeepSaves = -1 })
		a := e.Workspace("no-count-limit")
		as := saves(e, a.ID, 3)
		e.Clock.Advance(8 * 24 * time.Hour)
		Equal(e, "pruned with no count limit", prune(e), 2) // the newest is what the workspace stands on
		Equal(e, "newest save", stateOf(e, as[2].ID), "ready")

		e.RestartWith(func(c *engine.Config) { c.KeepSaves = 20; c.KeepSavesFor = -1 })
		b := e.Workspace("no-age-limit")
		bs := saves(e, b.ID, 23)
		Equal(e, "pruned with no age limit", prune(e), 3)
		Equal(e, "oldest of 23 young saves", stateOf(e, bs[0].ID), "deleted")

		e.RestartWith(func(c *engine.Config) { c.KeepSaves = -1; c.KeepSavesFor = -1 })
		cs := saves(e, e.Workspace("no-limits").ID, 3)
		e.Clock.Advance(30 * 24 * time.Hour)
		Equal(e, "pruned with both limits off", prune(e), 0)
		Equal(e, "oldest save with both limits off", stateOf(e, cs[0].ID), "ready")
	})
}
