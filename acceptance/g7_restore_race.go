package acceptance

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/AQLD/cairn/client"
)

// G7.16: restores running while other workspaces start never lose a file.
// Unmounting a workspace's disk image does not always end its filesystem at
// once (a sandbox starting at that moment copies every mount into its own
// namespace); mounting the image again before it ends read a filesystem that
// had not been written yet: files saved and restored were missing.

func init() {
	register("G7.16", func(e *Env) {
		const workers = 4
		cycles := max(20, Cycles())
		var wg sync.WaitGroup
		var stop atomic.Bool
		errs := make(chan error, workers+1)

		// churn: sandboxes starting and stopping beside the restores
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: fmt.Sprintf("churn-%d", i)})
				if err != nil {
					errs <- fmt.Errorf("churn create: %w", err)
					return
				}
				if _, err := e.C.Run(e.Ctx, ws.ID, "true"); err != nil {
					errs <- fmt.Errorf("churn run: %w", err)
					return
				}
				if err := e.C.DeleteWorkspace(e.Ctx, ws.ID); err != nil {
					errs <- fmt.Errorf("churn delete: %w", err)
					return
				}
			}
		}()

		var restorers sync.WaitGroup
		for w := range workers {
			restorers.Add(1)
			go func() {
				defer restorers.Done()
				ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: fmt.Sprintf("restorer-%d", w)})
				if err != nil {
					errs <- err
					return
				}
				for c := range cycles {
					want := fmt.Sprintf("worker %d cycle %d", w, c)
					if err := e.C.WriteFile(e.Ctx, ws.ID, "state.txt", []byte(want)); err != nil {
						errs <- err
						return
					}
					sv, err := e.C.Save(e.Ctx, ws.ID, client.QuickSave)
					if err != nil {
						errs <- err
						return
					}
					if err := e.C.WriteFile(e.Ctx, ws.ID, "state.txt", []byte("overwritten")); err != nil {
						errs <- err
						return
					}
					if _, err := e.C.Resume(e.Ctx, sv.ID); err != nil {
						errs <- err
						return
					}
					got, err := e.C.ReadFile(e.Ctx, ws.ID, "state.txt")
					if err != nil || string(got) != want {
						errs <- fmt.Errorf("worker %d cycle %d: after resume got %q, %v; want %q", w, c, got, err, want)
						return
					}
				}
			}()
		}
		restorers.Wait()
		stop.Store(true)
		wg.Wait()
		close(errs)
		for err := range errs {
			e.T.Error(err)
		}
	})
}
