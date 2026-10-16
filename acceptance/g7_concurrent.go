package acceptance

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AQLD/cairn/client"
)

// G7.10: concurrent operations never corrupt state.
//
// Workers share a small pool of workspaces and fire random operations at them
// at the same time. Some operations legitimately lose a race (exec while the
// workspace is resuming answers "conflict"); anything else is a failure. At the
// end every workspace must be running and every save must restore cleanly.

func init() { register("G7.10", concurrentOps) }

func concurrentOps(e *Env) {
	workers, opsPerWorker, pool := 16, 40, 4
	if os.Getenv("CAIRN_TEST_TEMPLATE") != "" {
		// every save carries a full template filesystem: keep the run within a test disk
		workers, opsPerWorker = 8, 15
	}
	var wss []string
	for i := 0; i < pool; i++ {
		wss = append(wss, e.Workspace(fmt.Sprintf("shared-%d", i)).ID)
	}

	var mu sync.Mutex
	var saves []string
	var unexpected []string
	allowed := func(err error) bool {
		// losing a race against a resume or full save is fine; everything else is not
		return err == nil || client.IsCode(err, "conflict")
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 99))
			for i := 0; i < opsPerWorker; i++ {
				ws := wss[rng.IntN(len(wss))]
				var err error
				switch op := rng.IntN(100); {
				case op < 35:
					err = e.C.WriteFile(e.Ctx, ws, fmt.Sprintf("w%d/f%d", w, i), []byte(strings.Repeat("x", rng.IntN(4096))))
				case op < 55:
					_, err = e.C.Exec(e.Ctx, ws, client.Exec{Command: []string{"sh", "-c", "ls -R . > /dev/null"}})
				case op < 75:
					var sv *client.Save
					if sv, err = e.C.Save(e.Ctx, ws, client.QuickSave); err == nil {
						mu.Lock()
						saves = append(saves, sv.ID)
						mu.Unlock()
					}
				case op < 88:
					mu.Lock()
					var id string
					if len(saves) > 0 {
						id = saves[rng.IntN(len(saves))]
					}
					mu.Unlock()
					if id != "" {
						_, err = e.C.Resume(e.Ctx, id)
					}
				default:
					mu.Lock()
					var id string
					if len(saves) > 0 {
						id = saves[rng.IntN(len(saves))]
					}
					mu.Unlock()
					if id != "" {
						_, err = e.C.Copy(e.Ctx, id, 1)
					}
				}
				if !allowed(err) {
					mu.Lock()
					unexpected = append(unexpected, err.Error())
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	if len(unexpected) > 0 {
		var failed []string
		for _, line := range strings.Split(e.Logs.String(), "\n") {
			if strings.Contains(line, "request failed") {
				failed = append(failed, line)
			}
		}
		e.T.Fatalf("%d unexpected errors, first: %s\nengine errors:\n%s", len(unexpected), unexpected[0], strings.Join(failed, "\n"))
	}

	list, err := e.C.ListWorkspaces(e.Ctx)
	e.Must(err)
	for _, w := range list {
		if w.State != "running" {
			e.T.Fatalf("workspace %s left %s", w.ID, w.State)
		}
	}
	for _, id := range saves {
		sv, err := e.C.GetSave(e.Ctx, id)
		e.Must(err)
		Equal(e, "save state", sv.State, "ready")
		cp, err := e.C.Copy(e.Ctx, id, 1)
		e.Must(err)
		e.Must(e.C.DeleteWorkspace(e.Ctx, cp[0].ID))
	}
	entries, _ := os.ReadDir(filepath.Join(e.DataDir, "saves"))
	for _, d := range entries {
		if strings.HasPrefix(d.Name(), ".tmp-") {
			e.T.Fatalf("half-written save left: %s", d.Name())
		}
	}
	e.T.Logf("%d workers x %d ops on %d shared workspaces; %d saves all restore", workers, opsPerWorker, pool, len(saves))
}
