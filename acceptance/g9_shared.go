package acceptance

import (
	"path/filepath"
	"strings"

	"github.com/AQLD/cairn/internal/engine"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/disk"
)

// G9.9: workspaces made from one template share one read-only copy of it and
// store only their own changes.

// sharedExtra makes the shared-template scenarios' template large next to
// what a workspace takes of its own even when empty (a fresh disk image's
// filesystem metadata is a few MB).
const sharedExtra = 64 << 20

// sharesTemplate fails unless workspace ws takes well under the template's
// size of its own: a copy of the template would take all of it.
func sharesTemplate(e *Env, what, ws string, t *client.Template) {
	e.T.Helper()
	if n := ownBytes(e, ws); n >= t.Size/4 {
		e.T.Fatalf("%s takes %d bytes of its own, from a %d byte template: want only its changes", what, n, t.Size)
	}
}

// templateFiles is a workspace's listing narrowed to what the template
// put there (opt/): runtimes add their own entries elsewhere (gVisor makes
// /dev/cairn-init when a sandbox starts), stamped with that start.
func templateFiles(e *Env, ws string) map[string]string {
	e.T.Helper()
	out := map[string]string{}
	for p, v := range fsListing(e, ws) {
		if p == "opt" || strings.HasPrefix(p, "opt/") {
			out[p] = v
		}
	}
	return out
}

// shareTemplates restarts the engine with shared templates switched on
// (they are off by default; see engine.Config.SharedTemplates).
func shareTemplates(e *Env) {
	e.T.Helper()
	e.RestartWith(func(c *engine.Config) { c.SharedTemplates = true })
}

// ownBytes is the disk a workspace takes of its own, as the engine reports.
func ownBytes(e *Env, ws string) int64 {
	e.T.Helper()
	st, err := e.C.Storage(e.Ctx)
	e.Must(err)
	for _, u := range st.PerWorkspace {
		if u.ID == ws {
			return u.Bytes
		}
	}
	e.T.Fatalf("workspace %s is not in the storage report", ws)
	return 0
}

func init() {
	register("G9.9.1", func(e *Env) {
		shareTemplates(e)
		t := deltaTemplateWith(e, "shared", sharedExtra)
		var wss []*client.Workspace
		for _, n := range []string{"one", "two", "three"} {
			ws := templatedWorkspace(e, "shared", n)
			e.Sh(ws.ID, `cd `+deltaDir+` && echo edited >> keep/edit.txt && echo new > lib/new.txt`)
			wss = append(wss, ws)
		}
		sv := e.Save(wss[0].ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		wss = append(wss, &copies[0])
		for _, ws := range wss {
			sharesTemplate(e, "workspace "+ws.Name, ws.ID, t)
		}
		Equal(e, "copy has the edit", e.Read(copies[0].ID, deltaDir+"/lib/new.txt"), "new\n")
	})

	register("G9.9.2", func(e *Env) {
		shareTemplates(e)
		deltaTemplate(e, "shared-iso")
		a := templatedWorkspace(e, "shared-iso", "a")
		b := templatedWorkspace(e, "shared-iso", "b")
		pristine := templateFiles(e, b.ID)

		e.Sh(a.ID, agentEdits)
		e.Sh(a.ID, `cd `+deltaDir+` && mv keep kept && chmod 600 big.bin && ln lib/b.txt lib/b-again.txt && echo x >> lib/b-again.txt`)

		sameListing(e, "a workspace next to the one that changed", templateFiles(e, b.ID), pristine)
		c := templatedWorkspace(e, "shared-iso", "c")
		sameListing(e, "a new workspace from the template", templateFiles(e, c.ID), pristine)
		Equal(e, "the hard link and its source are one file", e.Read(a.ID, deltaDir+"/lib/b.txt"), "lib b\nx\n")
	})

	register("G9.9.3", func(e *Env) {
		shareTemplates(e)
		t := deltaTemplateWith(e, "shared-saves", sharedExtra)
		ws := templatedWorkspace(e, "shared-saves", "work")
		e.Sh(ws.ID, agentEdits)
		e.Sh(ws.ID, `cd `+deltaDir+` && mv keep kept`)
		want := fsListing(e, ws.ID)
		sv := e.Save(ws.ID)

		e.Sh(ws.ID, `cd `+deltaDir+` && rm -rf lib kept && echo x > big.bin && echo junk > junk.txt`)
		e.Resume(sv.ID)
		sameListing(e, "workspace after resume", fsListing(e, ws.ID), want)

		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		sameListing(e, "copy of the save", fsListing(e, copies[0].ID), want)
		sv2 := e.Save(copies[0].ID)
		copies2, err := e.C.Copy(e.Ctx, sv2.ID, 1)
		e.Must(err)
		sameListing(e, "copy of the copy's save", fsListing(e, copies2[0].ID), want)
		for _, id := range []string{ws.ID, copies[0].ID, copies2[0].ID} {
			sharesTemplate(e, "restored workspace "+id, id, t)
		}
	})

	register("G9.9.5", func(e *Env) {
		shareTemplates(e)
		deltaTemplate(e, "shared-boot")
		ws := templatedWorkspace(e, "shared-boot", "boot")
		e.Sh(ws.ID, agentEdits)
		want := fsListing(e, ws.ID)

		e.Restart()
		sameListing(e, "after an engine restart", fsListing(e, ws.ID), want)

		// a host restart: sandboxes and engine stop, and every mount is gone
		e.Must(e.Engine.StopAll(e.Ctx))
		e.stop()
		dir := filepath.Join(e.DataDir, "workspaces", ws.ID)
		e.Must(disk.Unmount(filepath.Join(dir, "rootfs")))
		e.Must(disk.Unmount(filepath.Join(dir, "rootfs.layer")))
		e.start()
		sameListing(e, "after the mounts were lost", fsListing(e, ws.ID), want)
		Equal(e, "commands run", e.Sh(ws.ID, `cat `+deltaDir+`/lib/new.txt`), "new\n")
	})

	register("G9.9.8", func(e *Env) {
		e.RestartWith(func(c *engine.Config) { c.SharedTemplates = false })
		deltaTemplate(e, "shared-old")
		ws := templatedWorkspace(e, "shared-old", "old")
		e.Sh(ws.ID, agentEdits)
		pid := sleeper(e, ws.ID, "old")
		want := templateFiles(e, ws.ID)
		quick := e.Save(ws.ID)
		full, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)

		shareTemplates(e)
		if _, err := e.Engine.Resume(e.Ctx, full.ID); err != nil {
			e.T.Fatalf("full save from before sharing: %v", err)
		}
		Equal(e, "process of the full save", alive(e, ws.ID, pid), true)
		sameListing(e, "files of the full save", templateFiles(e, ws.ID), want)
		e.Resume(quick.ID)
		sameListing(e, "files of the quick save", templateFiles(e, ws.ID), want)
	})
}
