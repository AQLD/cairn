package acceptance

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
