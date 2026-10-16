package acceptance

import (
	"os"
	"path/filepath"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/runtime"
)

// G6.7: a full save made with one gVisor release resumes after the engine is
// upgraded to another. CAIRN_RUNSC_PREVIOUS names the older runsc (installed
// side by side, e.g. `sudo deploy/install-gvisor.sh 20260928`).

func init() {
	register("G6.7", func(e *Env) {
		previous := os.Getenv("CAIRN_RUNSC_PREVIOUS")
		if previous == "" {
			e.T.Skip("needs CAIRN_RUNSC_PREVIOUS: an older runsc installed next to the current one")
		}
		current, ok := e.rt.(*runtime.Runsc)
		if !ok {
			e.T.Skip("needs the runsc runtime")
		}
		useRunsc := func(bin string) {
			e.rt = runtime.NewRunsc(bin, filepath.Join(e.DataDir, "runsc"), current.Init)
			e.Restart()
		}

		useRunsc(previous) // the engine before the upgrade
		ws := e.Workspace("long-task")
		background(e, ws.ID, counterProgram)
		before := running(e, ws.ID)
		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)

		useRunsc(current.Binary) // the upgrade
		e.Resume(sv.ID)
		Equal(e, "process after the upgrade", running(e, ws.ID).PID, before.PID)
		// the workspace keeps working, and saves again, on the build it runs on
		e.Write(ws.ID, "after", "upgraded")
		again, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		e.Resume(again.ID)
		Equal(e, "process after a second full save", running(e, ws.ID).PID, before.PID)
		Equal(e, "files", e.Read(ws.ID, "after"), "upgraded")
	})
}
