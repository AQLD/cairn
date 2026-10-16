package acceptance

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
)

// G7.13: a flood of requests is turned away or queued, never answered with
// errors, and never takes the engine or other workspaces down.

func init() {
	register("G7.13", func(e *Env) {
		const maxCommands, maxOps = 4, 2
		e.RestartWith(func(c *engine.Config) { c.MaxCommands, c.MaxOps = maxCommands, maxOps })
		defer e.RestartWith(nil)
		busy := e.Workspace("busy")
		neighbour := e.Workspace("neighbour")

		// a burst of long commands: the limit runs, the rest are refused
		ctx, cancel := context.WithCancel(e.Ctx)
		var running, refused, failed atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := e.C.Exec(ctx, busy.ID, client.Exec{Command: []string{"sleep", "60"}, TimeoutMS: 120_000})
				var apiErr *client.Error
				switch {
				case errors.As(err, &apiErr) && apiErr.Code == "too_many_requests":
					refused.Add(1)
				case ctx.Err() != nil:
					running.Add(1) // ran until we cancelled it
				default:
					failed.Add(1)
					e.T.Errorf("command in the burst: %v", err)
				}
			}()
		}
		deadline := time.Now().Add(30 * time.Second)
		for refused.Load() < 50-maxCommands && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		Equal(e, "neighbour answers during the flood", e.Sh(neighbour.ID, "echo ok"), "ok\n")
		cancel()
		wg.Wait()
		Equal(e, "commands refused", int(refused.Load()), 50-maxCommands)
		Equal(e, "commands run", int(running.Load()), maxCommands)

		// once they are done, the workspace takes commands again
		deadline = time.Now().Add(10 * time.Second)
		for {
			out, err := e.C.Run(e.Ctx, busy.ID, "echo", "again")
			if err == nil {
				Equal(e, "after the flood", out, "again\n")
				break
			}
			if time.Now().After(deadline) {
				e.T.Fatalf("workspace still refuses commands after the flood: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}

		// heavy operations past the limit wait their turn rather than fail
		e.Write(busy.ID, "f", "data")
		var saves sync.WaitGroup
		for i := 0; i < 12; i++ {
			saves.Add(1)
			go func() {
				defer saves.Done()
				if _, err := e.C.Save(e.Ctx, busy.ID, client.QuickSave); err != nil {
					e.T.Errorf("save in a burst: %v", err)
				}
			}()
		}
		saves.Wait()
		list, err := e.C.ListSaves(e.Ctx, busy.ID)
		e.Must(err)
		Equal(e, "saves made", len(list), 12)
	})
}
