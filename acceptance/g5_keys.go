package acceptance

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/AQLD/cairn/client"
)

// G5.7: API keys. The engine runs with an operator token (the admin), who
// makes a key per caller.

// withAuth restarts the engine requiring the operator's token.
func withAuth(e *Env) {
	e.T.Helper()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	e.token = "operator-" + hex.EncodeToString(b)
	e.Restart()
}

// keyClient makes a key with a scope and returns a client using it.
func keyClient(e *Env, name, scope string) (*client.Client, *client.Key) {
	e.T.Helper()
	k, err := e.C.CreateKey(e.Ctx, name, scope)
	e.Must(err)
	if !strings.HasPrefix(k.Token, "cairn_") || k.Scope != scope {
		e.T.Fatalf("new key: %+v", k)
	}
	url, _ := e.endpoint()
	return client.New(url, k.Token), k
}

func init() {
	register("G5.7.1", func(e *Env) {
		withAuth(e)
		agent, k := keyClient(e, "agent-1", "write")
		ws, err := agent.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "by-key"})
		e.Must(err)
		out, err := agent.Run(e.Ctx, ws.ID, "echo", "hello")
		e.Must(err)
		Equal(e, "command through a key", strings.TrimSpace(out), "hello")
		list, err := e.C.ListKeys(e.Ctx)
		e.Must(err)
		if len(list) != 1 || list[0].ID != k.ID || list[0].Token != "" {
			e.T.Fatalf("listing: %+v", list)
		}
		e.Restart() // flush the store
		if p := scanFor(e, e.DataDir, k.Token); p != "" {
			e.T.Fatalf("%s holds the key's token", p)
		}
		// a made-up token is refused
		url, _ := e.endpoint()
		_, err = client.New(url, "cairn_"+strings.Repeat("0", 64)).ListWorkspaces(e.Ctx)
		e.ErrCode(err, "unauthorized")
	})

	register("G5.7.2", func(e *Env) {
		withAuth(e)
		a, ka := keyClient(e, "agent-a", "write")
		b, _ := keyClient(e, "agent-b", "write")
		_, err := a.ListWorkspaces(e.Ctx)
		e.Must(err)
		e.Must(e.C.RevokeKey(e.Ctx, ka.ID))
		_, err = a.ListWorkspaces(e.Ctx)
		e.ErrCode(err, "unauthorized")
		_, err = b.ListWorkspaces(e.Ctx)
		e.Must(err)
		_, err = e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		list, err := e.C.ListKeys(e.Ctx)
		e.Must(err)
		for _, k := range list {
			if (k.ID == ka.ID) != (k.RevokedAt != nil) {
				e.T.Fatalf("key %s revoked at %v", k.ID, k.RevokedAt)
			}
		}
		e.Must(e.C.RevokeKey(e.Ctx, ka.ID)) // again: fine
		e.ErrCode(e.C.RevokeKey(e.Ctx, "key_nonexistent"), "not_found")
	})

	register("G5.7.3", func(e *Env) {
		withAuth(e)
		ws := e.Workspace("watched")
		e.Write(ws.ID, "notes.txt", "before")
		sv := e.Save(ws.ID)
		reader, _ := keyClient(e, "dashboard", "read")
		if _, err := reader.ListWorkspaces(e.Ctx); err != nil {
			e.T.Fatalf("read key listing workspaces: %v", err)
		}
		got, err := reader.ReadFile(e.Ctx, ws.ID, "notes.txt")
		e.Must(err)
		Equal(e, "read key reads a file", string(got), "before")
		_, err = reader.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		forbidden := map[string]error{}
		_, forbidden["create"] = reader.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "nope"})
		_, forbidden["run"] = reader.Run(e.Ctx, ws.ID, "true")
		forbidden["write"] = reader.WriteFile(e.Ctx, ws.ID, "notes.txt", []byte("after"))
		_, forbidden["save"] = reader.Save(e.Ctx, ws.ID, client.QuickSave)
		_, forbidden["resume"] = reader.Resume(e.Ctx, sv.ID)
		forbidden["delete"] = reader.DeleteWorkspace(e.Ctx, ws.ID)
		for what, err := range forbidden {
			if !client.IsCode(err, "forbidden") {
				e.T.Fatalf("read key %s: %v, want forbidden", what, err)
			}
		}
		Equal(e, "nothing changed", e.Read(ws.ID, "notes.txt"), "before")
		all, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces", len(all), 1)
	})

	register("G5.7.4", func(e *Env) {
		withAuth(e)
		writer, _ := keyClient(e, "ci", "write")
		admin, _ := keyClient(e, "ops", "admin")
		ws, err := writer.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "work"})
		e.Must(err)
		_, err = writer.Save(e.Ctx, ws.ID, client.QuickSave)
		e.Must(err)
		e.Must(writer.DeleteWorkspace(e.Ctx, ws.ID))
		_, err = writer.CreateKey(e.Ctx, "escalate", "admin")
		e.ErrCode(err, "forbidden")
		_, err = writer.ListKeys(e.Ctx)
		e.ErrCode(err, "forbidden")
		_, err = writer.SetSecret(e.Ctx, "X", "v", []string{"example.com"})
		e.ErrCode(err, "forbidden")
		e.ErrCode(writer.DeleteTemplate(e.Ctx, "any"), "forbidden")
		if _, err := writer.ListTemplates(e.Ctx); err != nil {
			e.T.Fatalf("write key listing templates: %v", err)
		}
		k, err := admin.CreateKey(e.Ctx, "made-by-admin-key", "read")
		e.Must(err)
		e.Must(admin.RevokeKey(e.Ctx, k.ID))
	})
}
