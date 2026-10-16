package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/failpoint"
)

// G6.4: a full disk during a save fails cleanly.

func init() {
	register("G6.4.1", func(e *Env) {
		ws := e.Workspace("big")
		e.Write(ws.ID, "f", "data")
		// demand far more headroom than any disk has
		e.RestartWith(func(c *engine.Config) { c.MinFreeBytes = 1 << 62 })
		_, err := e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		e.ErrCode(err, "insufficient_storage")
		assertNoSaveLeftovers(e, ws.ID)
		e.RestartWith(nil)
		e.Save(ws.ID) // works again once there is room
	})

	register("G6.4.2", func(e *Env) {
		ws := e.Workspace("enospc")
		e.Sh(ws.ID, "head -c 1048576 /dev/urandom > blob")
		e.Failpoints.Enable(failpoint.SaveWrite, syscall.ENOSPC)
		_, err := e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		e.ErrCode(err, "insufficient_storage")
		e.Failpoints.Disable(failpoint.SaveWrite)
		assertNoSaveLeftovers(e, ws.ID)

		got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "workspace keeps running", got.State, "running")
		e.Write(ws.ID, "after", "still writable")
		sv := e.Save(ws.ID)
		e.Resume(sv.ID)
		Equal(e, "save after the failure works", e.Read(ws.ID, "after"), "still writable")
	})
}

// assertNoSaveLeftovers checks a failed save left no record and no files.
func assertNoSaveLeftovers(e *Env, ws string) {
	e.T.Helper()
	saves, err := e.C.ListSaves(e.Ctx, ws)
	e.Must(err)
	Equal(e, "saves after the failure", len(saves), 0)
	entries, _ := os.ReadDir(filepath.Join(e.DataDir, "saves"))
	for _, d := range entries {
		if strings.HasPrefix(d.Name(), ".tmp-") || d.IsDir() {
			e.T.Fatalf("failed save left %s on disk", d.Name())
		}
	}
}
