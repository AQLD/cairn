package acceptance

import (
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
)

func init() {
	// G7.11: an agent's dev server or long build never wedges the workspace.
	register("G7.11", func(e *Env) {
		e.RestartWith(func(c *engine.Config) { c.ExclusiveWait = time.Second })
		ws := e.Workspace("busy")
		quick, err := e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		e.Must(err)

		long := make(chan error, 1)
		go func() {
			_, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sleep", "60"}, TimeoutMS: 120_000})
			long <- err
		}()
		time.Sleep(500 * time.Millisecond) // let it start

		// needs the workspace idle: a prompt, clear answer
		start := time.Now()
		_, err = e.C.Resume(e.Ctx, quick.ID)
		e.ErrCode(err, "conflict")
		if d := time.Since(start); d > 5*time.Second {
			e.T.Fatalf("resume took %v to answer", d)
		}
		if e.Info.Capabilities.FullSave {
			_, err = e.C.Save(e.Ctx, ws.ID, client.FullSave)
			e.ErrCode(err, "conflict")
		}

		// meanwhile everything else keeps working
		start = time.Now()
		Equal(e, "other command", e.Sh(ws.ID, "echo still here"), "still here\n")
		_, err = e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		e.Must(err)
		if d := time.Since(start); d > 5*time.Second {
			e.T.Fatalf("commands and quick saves stalled for %v behind the long command", d)
		}

		// deleting the workspace ends the command instead of waiting for it
		start = time.Now()
		e.Must(e.C.DeleteWorkspace(e.Ctx, ws.ID))
		select {
		case <-long:
		case <-time.After(10 * time.Second):
			e.T.Fatal("the long command was still running 10s after its workspace was deleted")
		}
		if d := time.Since(start); d > 10*time.Second {
			e.T.Fatalf("delete took %v", d)
		}
	})
}
