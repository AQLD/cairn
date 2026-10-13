package acceptance

import (
	"os"
	"path/filepath"

	"github.com/AQLD/cairn/client"
)

// G3, G5, G8 and G10 scenarios that hold on every runtime.

func init() {
	// G3.5: workspaces are managed independently.
	register("G3.5", func(e *Env) {
		a := e.Workspace("a")
		b := e.Workspace("b")
		e.Write(a.ID, "f", "a")
		e.Write(b.ID, "f", "b")
		sv := e.Save(a.ID)
		e.Must(e.C.DeleteWorkspace(e.Ctx, a.ID))
		Equal(e, "b untouched by a's save and delete", e.Read(b.ID, "f"), "b")
		saves, err := e.C.ListSaves(e.Ctx, b.ID)
		e.Must(err)
		Equal(e, "b has no saves", len(saves), 0)
		list, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "listed", len(list), 1)
		Equal(e, "remaining", list[0].ID, b.ID)
		_ = sv
	})

	// G5.1: nothing but the data folder.
	register("G5.1", func(e *Env) {
		e.Need("in_process")
		ws := e.Workspace("self-contained")
		e.Write(ws.ID, "f", "x")
		e.Resume(e.Save(ws.ID).ID)
		// everything the engine wrote lives under the data folder
		for _, want := range []string{"state.db", "workspaces", "saves"} {
			if _, err := os.Stat(filepath.Join(e.DataDir, want)); err != nil {
				e.T.Fatalf("%s missing from the data folder: %v", want, err)
			}
		}
	})

	// G5.2: token required.
	register("G5.2", func(e *Env) {
		e.Need("in_process")
		secured := NewEnvWithToken(e.T, "s3cret-token")
		e.Must(secured.C.Health(secured.Ctx)) // health stays open for load balancers

		anon := client.New(secureURL(secured), "")
		_, err := anon.ListWorkspaces(secured.Ctx)
		secured.ErrCode(err, "unauthorized")
		wrong := client.New(secureURL(secured), "wrong")
		_, err = wrong.CreateWorkspace(secured.Ctx, client.CreateWorkspace{})
		secured.ErrCode(err, "unauthorized")

		ws := secured.Workspace("ok")
		Equal(secured, "authorized create", ws.State, "running")
	})

	// G8.1: the file API cannot reach outside the workspace.
	register("G8.1", func(e *Env) {
		ws := e.Workspace("traversal")
		e.Write(ws.ID, "inside.txt", "in")
		for _, p := range []string{"../../../../etc/passwd", "/etc/passwd", "a/../../../etc/hosts", `..\..\etc\passwd`} {
			data, err := e.C.ReadFile(e.Ctx, ws.ID, p)
			if err == nil && len(data) > 0 {
				e.T.Fatalf("read %q returned %d bytes from outside the workspace", p, len(data))
			}
		}
		// writes that try to climb out land inside the workspace or fail
		_ = e.C.WriteFile(e.Ctx, ws.ID, "../../outside.txt", []byte("x"))
		if e.DataDir != "" {
			if _, err := os.Stat(filepath.Join(e.DataDir, "workspaces", "outside.txt")); err == nil {
				e.T.Fatal("write escaped the workspace")
			}
		}
	})

	// G8.2: the file API does not follow symlinks out.
	register("G8.2", func(e *Env) {
		ws := e.Workspace("symlink")
		secret := filepath.Join(e.T.TempDir(), "host-secret.txt")
		e.Must(os.WriteFile(secret, []byte("host secret"), 0o600))
		e.Sh(ws.ID, "ln -s "+secret+" link.txt && ln -s / rootlink")
		for _, p := range []string{"link.txt", "rootlink" + secret} {
			data, err := e.C.ReadFile(e.Ctx, ws.ID, p)
			if err == nil {
				e.T.Fatalf("read through symlink %q returned %q", p, data)
			}
		}
		if err := e.C.WriteFile(e.Ctx, ws.ID, "link.txt", []byte("overwritten")); err == nil {
			data, _ := os.ReadFile(secret)
			if string(data) != "host secret" {
				e.T.Fatal("write through symlink changed a host file")
			}
		}
	})

	// G10.1: health check.
	register("G10.1", func(e *Env) {
		e.Must(e.C.Health(e.Ctx))
	})

	// G10.2: capabilities reported honestly.
	register("G10.2", func(e *Env) {
		if e.Info.Runtime == "" || e.Info.SaveFormat < 1 {
			e.T.Fatalf("incomplete info: %+v", e.Info)
		}
		if e.Info.Runtime == "host" && (e.Info.Capabilities.Isolation || e.Info.Capabilities.ResourceLimits || e.Info.Capabilities.NetworkControl) {
			e.T.Fatal("host runtime claims guarantees it cannot give")
		}
		// a full save is refused, not faked, when the runtime cannot do it
		if !e.Info.Capabilities.FullSave {
			ws := e.Workspace("caps")
			_, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
			e.ErrCode(err, "unsupported_by_runtime")
		}
	})

	// G10.3: stable error codes.
	register("G10.3", func(e *Env) {
		_, err := e.C.GetWorkspace(e.Ctx, "ws_doesnotexist")
		e.ErrCode(err, "not_found")
		_, err = e.C.Resume(e.Ctx, "sv_doesnotexist")
		e.ErrCode(err, "not_found")
		ws := e.Workspace("codes")
		_, err = e.C.Exec(e.Ctx, ws.ID, client.Exec{})
		e.ErrCode(err, "invalid_argument")
		_, err = e.C.Copy(e.Ctx, e.Save(ws.ID).ID, 0)
		e.ErrCode(err, "invalid_argument")
		_, err = e.C.Save(e.Ctx, ws.ID, "sideways")
		e.ErrCode(err, "invalid_argument")
	})
}

func secureURL(e *Env) string { return e.srv.URL }
