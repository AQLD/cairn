package acceptance

import (
	"strconv"
	"strings"
	"sync"

	"github.com/AQLD/cairn/client"
)

// G2: try several fixes at once.

func init() {
	register("G2.1", func(e *Env) {
		ws := e.Workspace("source")
		e.Write(ws.ID, "repo/app.py", "v1")
		sv := e.Save(ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 4)
		e.Must(err)
		Equal(e, "copies", len(copies), 4)
		for _, c := range copies {
			Equal(e, "copy content", e.Read(c.ID, "repo/app.py"), "v1")
			Equal(e, "source save", c.SourceSave, sv.ID)
			Equal(e, "state", c.State, "running")
		}
	})

	register("G2.2", func(e *Env) {
		ws := e.Workspace("source")
		e.Write(ws.ID, "fix.txt", "original")
		sv := e.Save(ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 3)
		e.Must(err)
		for i, c := range copies {
			e.Write(c.ID, "fix.txt", "attempt "+strconv.Itoa(i))
		}
		for i, c := range copies {
			Equal(e, "copy keeps its own write", e.Read(c.ID, "fix.txt"), "attempt "+strconv.Itoa(i))
		}
		Equal(e, "original untouched", e.Read(ws.ID, "fix.txt"), "original")
	})

	register("G2.3", func(e *Env) {
		ws := e.Workspace("source")
		sv := e.Save(ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 5)
		e.Must(err)
		seen := map[string]bool{ws.ID: true}
		for _, c := range copies {
			if seen[c.ID] {
				e.T.Fatalf("duplicate workspace id %s", c.ID)
			}
			seen[c.ID] = true
			id := strings.TrimSpace(e.Sh(c.ID, `echo "$CAIRN_WORKSPACE_ID"`))
			Equal(e, "identity inside the copy", id, c.ID)
			host := strings.TrimSpace(e.Sh(c.ID, `echo "$HOSTNAME"`))
			Equal(e, "hostname inside the copy", host, c.ID)
		}
	})

	register("G2.4", func(e *Env) {
		ws := e.Workspace("source")
		e.Write(ws.ID, "task.sh", "echo working on $CAIRN_WORKSPACE_ID > result.txt\n")
		sv := e.Save(ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 20)
		e.Must(err)
		Equal(e, "copies", len(copies), 20)

		// all twenty work at the same time
		var wg sync.WaitGroup
		errs := make(chan error, len(copies))
		for _, c := range copies {
			wg.Add(1)
			go func(c client.Workspace) {
				defer wg.Done()
				if _, err := e.C.Run(e.Ctx, c.ID, "sh", "task.sh"); err != nil {
					errs <- err
				}
			}(c)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			e.T.Fatal(err)
		}
		for _, c := range copies {
			Equal(e, "result", e.Read(c.ID, "result.txt"), "working on "+c.ID+"\n")
		}
	})

	register("G2.5", func(e *Env) {
		ws := e.Workspace("gen0")
		e.Write(ws.ID, "lineage.txt", "gen0\n")
		gen1, err := e.C.Copy(e.Ctx, e.Save(ws.ID).ID, 1)
		e.Must(err)
		e.Sh(gen1[0].ID, "echo gen1 >> lineage.txt")
		gen2, err := e.C.Copy(e.Ctx, e.Save(gen1[0].ID).ID, 2)
		e.Must(err)
		for _, c := range gen2 {
			Equal(e, "lineage", e.Read(c.ID, "lineage.txt"), "gen0\ngen1\n")
		}
	})

	register("G2.7", func(e *Env) {
		ws := e.Workspace("source")
		e.Write(ws.ID, "counter", "0")
		sv := e.Save(ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 3)
		e.Must(err)
		e.Write(ws.ID, "counter", "1")
		Equal(e, "original still works", e.Read(ws.ID, "counter"), "1")
		for _, c := range copies {
			Equal(e, "copy unaffected", e.Read(c.ID, "counter"), "0")
		}
		got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "original state", got.State, "running")
	})
}
