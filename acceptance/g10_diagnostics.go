package acceptance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
)

// G10.9: a diagnostics bundle with facts, never contents.

// bundle fetches the diagnostics and returns its files by name.
func bundle(e *Env, c *client.Client) map[string][]byte {
	e.T.Helper()
	var buf bytes.Buffer
	e.Must(c.Diagnostics(e.Ctx, &buf))
	gz, err := gzip.NewReader(&buf)
	e.Must(err)
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		e.Must(err)
		data, err := io.ReadAll(tr)
		e.Must(err)
		files[strings.TrimPrefix(h.Name, "cairn-diagnostics/")] = data
	}
	return files
}

func init() {
	register("G10.9.1", func(e *Env) {
		ws := e.Workspace("diag")
		e.Save(ws.ID)
		e.Sh(ws.ID, "true")
		files := bundle(e, e.C)
		for _, name := range []string{"README.json", "engine.json", "host.json", "config.json", "counts.json", "workspaces.json", "saves.json", "activity.json", "checks.json"} {
			var v any
			if err := json.Unmarshal(files[name], &v); err != nil {
				e.T.Fatalf("%s: not readable JSON: %v", name, err)
			}
		}
		var eng struct {
			Info struct{ Runtime string } `json:"info"`
		}
		e.Must(json.Unmarshal(files["engine.json"], &eng))
		Equal(e, "runtime in the bundle", eng.Info.Runtime, e.Info.Runtime)
		var wss []struct{ ID, State string }
		e.Must(json.Unmarshal(files["workspaces.json"], &wss))
		if len(wss) != 1 || wss[0].ID != ws.ID || wss[0].State != "running" {
			e.T.Fatalf("workspaces.json: %+v", wss)
		}
		var saves []struct{ ID, Workspace string }
		e.Must(json.Unmarshal(files["saves.json"], &saves))
		if len(saves) != 1 || saves[0].Workspace != ws.ID {
			e.T.Fatalf("saves.json: %+v", saves)
		}
		if !strings.Contains(string(files["activity.json"]), `"exec"`) {
			e.T.Fatalf("activity.json has no exec count: %s", files["activity.json"])
		}
		if !strings.Contains(string(files["checks.json"]), `"state_store": "ok"`) {
			e.T.Fatalf("checks.json: %s", files["checks.json"])
		}
	})

	register("G10.9.2", func(e *Env) {
		sk := secretsKey(e)
		e.configure = func(c *engine.Config) { c.SecretsKey = sk }
		defer func() { e.configure = nil }()
		withAuth(e)
		canary := func(what string) string { return "canary-" + what + "-" + secretValue()[3:15] }
		name, path, content, command, output, envv, secret := canary("name"), canary("path"), canary("content"), canary("cmd"), canary("out"), canary("env"), canary("secret")
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: name})
		e.Must(err)
		e.Must(e.C.WriteFile(e.Ctx, ws.ID, path+".txt", []byte(content)))
		_, err = e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sh", "-c", "echo " + output + " # " + command}, Env: map[string]string{"PLANTED": envv}})
		e.Must(err)
		e.Save(ws.ID)
		_, err = e.C.SetSecret(e.Ctx, "PLANTED_SECRET", secret, []string{"example.com"})
		e.Must(err)
		k, err := e.C.CreateKey(e.Ctx, "planted-key", "write")
		e.Must(err)
		files := bundle(e, e.C)
		var all strings.Builder
		for _, data := range files {
			all.Write(data)
		}
		for what, s := range map[string]string{"workspace name": name, "file path": path, "file content": content,
			"command": command, "output": output, "environment": envv, "secret value": secret, "key token": k.Token, "key name": "planted-key"} {
			if strings.Contains(all.String(), s) {
				e.T.Fatalf("the bundle holds a %s", what)
			}
		}
		// only admins
		url, _ := e.endpoint()
		err = client.New(url, k.Token).Diagnostics(e.Ctx, io.Discard)
		e.ErrCode(err, "forbidden")
	})
}
