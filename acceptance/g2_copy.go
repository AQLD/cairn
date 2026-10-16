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

func init() {
	// G2.6: copies of a full save are distinct machines: their own hostname,
	// their own address, their own kernel randomness, and working network.
	register("G2.6", func(e *Env) {
		src, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "source", Network: true})
		e.Must(err)
		identity := `python3 -c "
import os, socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
try:
    s.connect(('1.1.1.1', 53)); ip = s.getsockname()[0]
except OSError as e:
    ip = 'none'
print(socket.gethostname(), ip, os.urandom(8).hex())"`
		online := `python3 -c "
import socket
try:
    socket.create_connection(('1.1.1.1', 443), timeout=5); print('online')
except OSError as e:
    print('offline', e)"`
		e.Sh(src.ID, "true")
		sv, err := e.C.Save(e.Ctx, src.ID, client.FullSave)
		e.Must(err)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 3)
		e.Must(err)
		e.Resume(sv.ID) // the original runs alongside its copies

		ips, rnd := map[string]string{}, map[string]string{}
		for _, ws := range append([]client.Workspace{{ID: src.ID}}, copies...) {
			f := strings.Fields(e.Sh(ws.ID, identity))
			if len(f) != 3 {
				e.T.Fatalf("%s: identity %q", ws.ID, f)
			}
			Equal(e, ws.ID+" hostname", f[0], ws.ID)
			for what, seen := range map[string]map[string]string{"address": ips, "randomness": rnd} {
				v := f[1]
				if what == "randomness" {
					v = f[2]
				}
				if other, dup := seen[v]; dup {
					e.T.Fatalf("%s and %s share the %s %s", other, ws.ID, what, v)
				}
				seen[v] = ws.ID
			}
			Equal(e, ws.ID+" network", e.Sh(ws.ID, online), "online\n")
		}
	})
}
