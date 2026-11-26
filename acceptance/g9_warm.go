package acceptance

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
)

// G9.10: a create returns at once from a warm pool.

// warmPool restarts the engine with a pool of size n for template.
func warmPool(e *Env, template string, n int) {
	e.T.Helper()
	e.RestartWith(func(c *engine.Config) { c.Warm = map[string]int{template: n} })
}

// waitWarm waits until template has n warm workspaces and returns their IDs.
func waitWarm(e *Env, template string, n int) []string {
	e.T.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		ids, err := e.Engine.WarmIDs(e.Ctx, template)
		e.Must(err)
		if len(ids) == n {
			return ids
		}
		if time.Now().After(deadline) {
			e.T.Fatalf("warm pool of %s: %d warm workspaces after a minute, want %d", template, len(ids), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func listed(e *Env) []string {
	e.T.Helper()
	ws, err := e.C.ListWorkspaces(e.Ctx)
	e.Must(err)
	var ids []string
	for _, w := range ws {
		ids = append(ids, w.ID)
	}
	return ids
}

func init() {
	register("G9.10.1", func(e *Env) {
		deltaTemplate(e, "warm")
		warmPool(e, "warm", 2)
		warm := waitWarm(e, "warm", 2)
		Equal(e, "workspaces listed before any create", len(listed(e)), 0)

		a, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "agent-a", Template: "warm"})
		e.Must(err)
		b, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "warm"})
		e.Must(err)
		if !slices.Contains(warm, a.ID) || !slices.Contains(warm, b.ID) || a.ID == b.ID {
			e.T.Fatalf("creates %s and %s did not each claim a warm workspace of %v", a.ID, b.ID, warm)
		}
		evs, err := e.C.Events(e.Ctx, a.ID, client.EventQuery{})
		e.Must(err)
		if len(evs) != 1 || evs[0].Kind != "create" {
			e.T.Fatalf("history of a workspace from the pool: %+v, want only its create", evs)
		}
		Equal(e, "name given", a.Name, "agent-a")
		Equal(e, "no name: named after itself", b.Name, b.ID)
		Equal(e, "state", a.State, "running")
		Equal(e, "listed after the creates", len(listed(e)), 2)
		Equal(e, "template's files", e.Read(a.ID, deltaDir+"/same.txt"), "AAAA")

		e.Write(a.ID, deltaDir+"/mine.txt", "only in a")
		if _, err := e.C.ReadFile(e.Ctx, b.ID, deltaDir+"/mine.txt"); err == nil {
			e.T.Fatal("a file written in one pooled workspace shows up in the other")
		}
		e.Sh(a.ID, "echo works")
	})

	register("G9.10.2", func(e *Env) {
		deltaTemplate(e, "warm")
		warmPool(e, "warm", 1)
		waitWarm(e, "warm", 1)
		for range 3 {
			_, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "warm"})
			e.Must(err)
		}
		Equal(e, "workspaces listed", len(listed(e)), 3)
		waitWarm(e, "warm", 1) // refilled
	})

	register("G9.10.3", func(e *Env) {
		deltaTemplate(e, "warm")
		warmPool(e, "warm", 2)
		waitWarm(e, "warm", 2)

		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, d := range []string{"opt/", deltaDir + "/"} {
			e.Must(tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755}))
		}
		e.Must(tw.WriteHeader(&tar.Header{Name: deltaDir + "/same.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}))
		_, err := tw.Write([]byte("NEW!"))
		e.Must(err)
		e.Must(tw.Close())
		_, err = e.C.ImportTemplate(e.Ctx, "warm", &buf)
		e.Must(err)

		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "warm"})
		e.Must(err)
		Equal(e, "content right after the template was replaced", e.Read(ws.ID, deltaDir+"/same.txt"), "NEW!")
		waitWarm(e, "warm", 2)
		ws, err = e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "warm"})
		e.Must(err)
		Equal(e, "content from the refilled pool", e.Read(ws.ID, deltaDir+"/same.txt"), "NEW!")

		e.Must(e.C.DeleteTemplate(e.Ctx, "warm"))
		time.Sleep(time.Second) // a refill that might run
		ids, err := e.Engine.WarmIDs(e.Ctx, "warm")
		e.Must(err)
		Equal(e, "warm workspaces of a deleted template", len(ids), 0)
	})

	register("G9.10.4", func(e *Env) {
		deltaTemplate(e, "warm")
		warmPool(e, "warm", 2)
		old := waitWarm(e, "warm", 2)
		for _, id := range old {
			if slices.Contains(listed(e), id) {
				e.T.Fatalf("warm workspace %s is listed", id)
			}
			_, err := e.C.GetWorkspace(e.Ctx, id)
			e.ErrCode(err, "not_found")
			_, err = e.C.Run(e.Ctx, id, "true")
			e.ErrCode(err, "not_found")
		}
		st, err := e.C.Storage(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces in the storage report", len(st.PerWorkspace), 0)

		e.Restart() // the pool's configuration stays
		fresh := waitWarm(e, "warm", 2)
		for _, id := range old {
			if slices.Contains(fresh, id) {
				e.T.Fatalf("warm workspace %s outlived the engine", id)
			}
			if _, err := os.Stat(filepath.Join(e.DataDir, "workspaces", id)); !os.IsNotExist(err) {
				e.T.Fatalf("files of warm workspace %s outlived the engine", id)
			}
		}
	})
}

// warmupServer starts a server in the background, notes its pid and a
// marker, and returns: what a warm-up does (a language server, a database).
func warmupServer(marker string) []string {
	return []string{"sh", "-c", "nohup python3 -m http.server 9000 >/dev/null 2>&1 & echo $! > /tmp/warm.pid; echo " + marker + " > /tmp/warm.marker; " +
		"for i in $(seq 50); do python3 -c \"import urllib.request; urllib.request.urlopen('http://127.0.0.1:9000/', timeout=1)\" 2>/dev/null && exit 0; sleep 0.2; done; exit 1"}
}

// serverUp reports whether the warm-up's server answers in ws.
func serverUp(e *Env, ws string) bool {
	e.T.Helper()
	res, err := e.C.Exec(e.Ctx, ws, client.Exec{Command: []string{"python3", "-c",
		"import urllib.request; urllib.request.urlopen('http://127.0.0.1:9000/', timeout=2)"}, TimeoutMS: 20_000})
	e.Must(err)
	return res.ExitCode == 0
}

func init() {
	register("G9.10.5", func(e *Env) {
		deltaTemplate(e, "warmed")
		t, err := e.C.SetTemplateWarmup(e.Ctx, "warmed", warmupServer("v1"))
		e.Must(err)
		if len(t.Warmup) == 0 {
			e.T.Fatalf("template does not show its warm-up: %+v", t)
		}
		warmPool(e, "warmed", 1)
		warm := waitWarm(e, "warmed", 1)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "warmed"})
		e.Must(err)
		if !slices.Contains(warm, ws.ID) {
			e.T.Fatal("the create did not come from the warm pool")
		}
		// the server answers at once: it was started by the warm-up, not now
		if !serverUp(e, ws.ID) {
			e.T.Fatal("the warm-up's server is not running in the pooled workspace")
		}
		started := e.Sh(ws.ID, "kill -0 $(cat /tmp/warm.pid) && echo alive")
		Equal(e, "warm-up process", strings.TrimSpace(started), "alive")
		Equal(e, "template's files", e.Read(ws.ID, deltaDir+"/same.txt"), "AAAA")
		Equal(e, "warm-up's marker", strings.TrimSpace(e.Read(ws.ID, "tmp/warm.marker")), "v1")
	})

	register("G9.10.6", func(e *Env) {
		deltaTemplate(e, "warmed")
		_, err := e.C.SetTemplateWarmup(e.Ctx, "warmed", warmupServer("v1"))
		e.Must(err)
		warmPool(e, "warmed", 2)
		waitWarm(e, "warmed", 2)
		a, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "warmed"})
		e.Must(err)
		b, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "warmed"})
		e.Must(err)
		ha, hb := strings.TrimSpace(e.Sh(a.ID, "hostname")), strings.TrimSpace(e.Sh(b.ID, "hostname"))
		if ha == hb || ha != a.ID || hb != b.ID {
			e.T.Fatalf("hostnames %q and %q, want each its own ID", ha, hb)
		}
		e.Write(a.ID, "only-a.txt", "a")
		if _, err := e.C.ReadFile(e.Ctx, b.ID, "only-a.txt"); err == nil {
			e.T.Fatal("a file one warmed workspace wrote is in the other")
		}
		for _, w := range []string{a.ID, b.ID} {
			evs, err := e.C.Events(e.Ctx, w, client.EventQuery{})
			e.Must(err)
			for _, ev := range evs {
				if ev.Kind == "exec" && strings.Contains(ev.Summary, "http.server") {
					e.T.Fatalf("the warm-up is in %s's history", w)
				}
			}
		}
		if !serverUp(e, a.ID) || !serverUp(e, b.ID) {
			e.T.Fatal("the warm-up's server is not running in both")
		}
	})

	register("G9.10.7", func(e *Env) {
		deltaTemplate(e, "warmed")
		_, err := e.C.SetTemplateWarmup(e.Ctx, "warmed", warmupServer("v1"))
		e.Must(err)
		warmPool(e, "warmed", 1)
		old := waitWarm(e, "warmed", 1)
		_, err = e.C.SetTemplateWarmup(e.Ctx, "warmed", warmupServer("v2"))
		e.Must(err)
		waitRewarmed := func(prev []string) []string {
			e.T.Helper()
			deadline := time.Now().Add(120 * time.Second)
			for {
				ids, err := e.Engine.WarmIDs(e.Ctx, "warmed")
				e.Must(err)
				if len(ids) == 1 && !slices.Contains(prev, ids[0]) {
					return ids
				}
				if time.Now().After(deadline) {
					e.T.Fatalf("the pool was not rebuilt after the change (warm %v, before %v)", ids, prev)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		fresh := waitRewarmed(old)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "warmed"})
		e.Must(err)
		if !slices.Contains(fresh, ws.ID) {
			e.T.Fatal("the create was not from the rebuilt pool")
		}
		Equal(e, "after the warm-up changed", strings.TrimSpace(e.Read(ws.ID, "tmp/warm.marker")), "v2")
		// the template replaced: the warm-up stays, the content is new
		deltaTemplateWith(e, "warmed", 4096)
		fresh2 := waitRewarmed(fresh)
		ws2, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "warmed"})
		e.Must(err)
		if !slices.Contains(fresh2, ws2.ID) {
			e.T.Fatal("the create after replacing the template was not from the rebuilt pool")
		}
		if !serverUp(e, ws2.ID) {
			e.T.Fatal("the warm-up did not run on the replaced template")
		}
		if _, err := e.C.ReadFile(e.Ctx, ws2.ID, "opt/extra.bin"); err != nil {
			e.T.Fatalf("a workspace from the pool after replacing the template lacks the new content: %v", err)
		}
	})
}
