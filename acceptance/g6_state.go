package acceptance

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/AQLD/cairn/internal/engine"
)

// G6.11: a damaged state database is detected, never acted on, and rebuilt from
// its last backup plus the data folder: workspaces and saves made after the
// backup come back, ones deleted after it stay deleted.

func init() {
	register("G6.11", func(e *Env) {
		kept := e.Workspace("before-backup")
		e.Write(kept.ID, "notes.txt", "from before the backup")
		keptSave := e.Save(kept.ID)
		gone := e.Workspace("deleted-after-backup")
		e.Must(e.Engine.BackupState(e.Ctx))

		later := e.Workspace("after-backup")
		e.Write(later.ID, "notes.txt", "from after the backup")
		laterSave := e.Save(later.ID)
		e.Must(e.C.DeleteWorkspace(e.Ctx, gone.ID))

		// the disk damages the database while the engine is down
		e.stop()
		damageFile(e, filepath.Join(e.DataDir, "state.db"))
		cfg := engine.Config{DataDir: e.DataDir, Runtime: e.rt, Now: e.Clock.Now}
		if eng, err := engine.Open(e.Ctx, cfg); err == nil {
			eng.Close()
			e.T.Fatal("the engine started on a damaged state database")
		} else if !strings.Contains(err.Error(), "--restore-state") {
			e.T.Fatalf("the error should say how to recover: %v", err)
		}

		e.configure = func(c *engine.Config) { c.RestoreState = true }
		e.start()
		e.configure = nil

		ws, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		names := map[string]string{}
		for _, w := range ws {
			names[w.ID] = w.Name
		}
		Equal(e, "workspace from before the backup", names[kept.ID], "before-backup")
		Equal(e, "workspace from after the backup", names[later.ID], "after-backup")
		if _, ok := names[gone.ID]; ok {
			e.T.Fatal("a workspace deleted after the backup came back")
		}
		Equal(e, "files", e.Read(later.ID, "notes.txt"), "from after the backup")
		for ws, sv := range map[string]string{kept.ID: keptSave.ID, later.ID: laterSave.ID} {
			saves, err := e.C.ListSaves(e.Ctx, ws)
			e.Must(err)
			if len(saves) != 1 || saves[0].ID != sv {
				e.T.Fatalf("saves of %s after the rebuild: %+v, want %s", ws, saves, sv)
			}
		}
		e.Write(later.ID, "notes.txt", "changed")
		e.Resume(laterSave.ID)
		Equal(e, "resume after the rebuild", e.Read(later.ID, "notes.txt"), "from after the backup")
		Equal(e, "commands", e.Sh(kept.ID, "cat notes.txt"), "from before the backup")

		e.Restart() // and the rebuilt database is an ordinary one from now on
		Equal(e, "after a normal restart", e.Read(kept.ID, "notes.txt"), "from before the backup")

		// left switched on, restoring does nothing to a healthy database: a
		// workspace no backup has seen is still there after a restart
		fresh := e.Workspace("after-the-rebuild")
		e.RestartWith(func(c *engine.Config) { c.RestoreState = true })
		got, err := e.C.GetWorkspace(e.Ctx, fresh.ID)
		e.Must(err)
		Equal(e, "workspace after a restart with restore on", got.Name, "after-the-rebuild")
		e.RestartWith(nil)
	})
}

// damageFile overwrites part of a file with garbage, as a failing disk might.
func damageFile(e *Env, path string) {
	e.T.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	e.Must(err)
	defer f.Close()
	garbage := []byte(strings.Repeat("\xde\xad\xbe\xef", 1024))
	_, err = f.WriteAt(garbage, 4096) // the second page: the first table's
	e.Must(err)
}
