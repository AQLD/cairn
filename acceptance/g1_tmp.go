package acceptance

import "github.com/AQLD/cairn/client"

func init() {
	// G1.11: /tmp is part of the workspace's disk, not throwaway memory.
	register("G1.11", func(e *Env) {
		ws := e.Workspace("scratch")
		e.Sh(ws.ID, "mkdir -p /tmp/work && echo draft-1 > /tmp/work/notes")
		sv, err := e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		e.Must(err)
		e.Sh(ws.ID, "echo draft-2 > /tmp/work/notes")
		e.Resume(sv.ID)
		Equal(e, "after resume", e.Sh(ws.ID, "cat /tmp/work/notes"), "draft-1\n")

		e.Restart()
		Equal(e, "after engine restart", e.Sh(ws.ID, "cat /tmp/work/notes"), "draft-1\n")
		Equal(e, "sticky bit kept", e.Sh(ws.ID, "stat -c %a /tmp"), "1777\n")
	})
}
