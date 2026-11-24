package acceptance

import (
	"strings"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
)

// G5.8: every API action is recorded with the key that made it.

// auditOf returns the audit's events, optionally for one key.
func auditOf(e *Env, key string) []client.Event {
	e.T.Helper()
	evs, err := e.C.Audit(e.Ctx, key, client.EventQuery{Limit: 1000})
	e.Must(err)
	return evs
}

// keyed reports whether events hold one of kind in ws by key.
func keyed(evs []client.Event, kind, ws, key string) bool {
	for _, ev := range evs {
		if ev.Kind == kind && ev.WorkspaceID == ws && ev.Key == key {
			return true
		}
	}
	return false
}

func init() {
	register("G5.8.1", func(e *Env) {
		withAuth(e)
		a, ka := keyClient(e, "agent-a", "write")
		b, kb := keyClient(e, "agent-b", "write")
		wa, err := a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "a"})
		e.Must(err)
		_, err = a.Run(e.Ctx, wa.ID, "echo", "from-a")
		e.Must(err)
		wb, err := b.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "b"})
		e.Must(err)
		e.Must(b.WriteFile(e.Ctx, wb.ID, "f.txt", []byte("x")))
		_, err = b.Save(e.Ctx, wb.ID, client.QuickSave)
		e.Must(err)
		e.Sh(wa.ID, "echo by-operator") // the operator's own token
		e.Must(b.DeleteWorkspace(e.Ctx, wb.ID))

		evs := auditOf(e, "")
		for _, want := range []struct{ kind, ws, key string }{
			{"create", wa.ID, ka.ID}, {"exec", wa.ID, ka.ID},
			{"create", wb.ID, kb.ID}, {"file_write", wb.ID, kb.ID}, {"save", wb.ID, kb.ID}, {"delete", wb.ID, kb.ID},
			{"exec", wa.ID, engine.OperatorKey},
		} {
			if !keyed(evs, want.kind, want.ws, want.key) {
				e.T.Fatalf("no %s in %s by %s in the audit: %+v", want.kind, want.ws, want.key, evs)
			}
		}
		// and the workspace's own log says who too
		wevs, err := e.C.Events(e.Ctx, wa.ID, client.EventQuery{})
		e.Must(err)
		if !keyed(wevs, "exec", wa.ID, ka.ID) {
			e.T.Fatalf("the workspace's log does not name the key: %+v", wevs)
		}
	})

	register("G5.8.2", func(e *Env) {
		sk := secretsKey(e)
		e.configure = func(c *engine.Config) { c.SecretsKey = sk }
		defer func() { e.configure = nil }()
		withAuth(e)
		a, ka := keyClient(e, "agent-a", "write")
		_, kb := keyClient(e, "agent-b", "write")
		admin, kadm := keyClient(e, "ops", "admin")
		for _, name := range []string{"one", "two"} {
			ws, err := a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: name})
			e.Must(err)
			_, err = a.Run(e.Ctx, ws.ID, "true")
			e.Must(err)
		}
		// one key's actions across its workspaces, and nobody else's
		mine := auditOf(e, ka.ID)
		if len(mine) < 4 {
			e.T.Fatalf("agent-a's audit: %+v", mine)
		}
		for _, ev := range mine {
			if ev.Key != ka.ID {
				e.T.Fatalf("filtered audit holds %s's action: %+v", ev.Key, ev)
			}
		}
		// engine actions by who made them
		_, err := admin.SetSecret(e.Ctx, "X", "v", []string{"example.com"})
		e.Must(err)
		e.Must(admin.RevokeKey(e.Ctx, kb.ID))
		all := auditOf(e, "")
		if !keyed(all, "key_create", "", engine.OperatorKey) {
			e.T.Fatalf("key creation by the operator is not in the audit: %+v", all)
		}
		if !keyed(all, "secret_set", "", kadm.ID) {
			e.T.Fatalf("the secret set by the admin key is not in the audit: %+v", all)
		}
		if !keyed(all, "key_revoke", "", kadm.ID) {
			e.T.Fatalf("the revocation by the admin key is not in the audit: %+v", all)
		}
		// only admins read it
		_, err = a.Audit(e.Ctx, "", client.EventQuery{})
		e.ErrCode(err, "forbidden")
		reader, _ := keyClient(e, "dashboard", "read")
		_, err = reader.Audit(e.Ctx, "", client.EventQuery{})
		e.ErrCode(err, "forbidden")
		if _, err := admin.Audit(e.Ctx, "", client.EventQuery{}); err != nil {
			e.T.Fatalf("an admin key reading the audit: %v", err)
		}
	})

	register("G5.8.3", func(e *Env) {
		withAuth(e)
		a, ka := keyClient(e, "agent-a", "write")
		ws, err := a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "short-lived"})
		e.Must(err)
		_, err = a.Run(e.Ctx, ws.ID, "echo", "kept")
		e.Must(err)
		e.Must(a.DeleteWorkspace(e.Ctx, ws.ID))
		e.Restart()
		evs := auditOf(e, ka.ID)
		for _, kind := range []string{"create", "exec", "delete"} {
			if !keyed(evs, kind, ws.ID, ka.ID) {
				e.T.Fatalf("after a restart and the workspace's deletion, %s by %s is gone: %+v", kind, ka.ID, evs)
			}
		}
	})

	register("G15.4", func(e *Env) {
		withAuth(e)
		nightly, kn := keyClient(e, "nightly-report", "write")
		adhoc, kh := keyClient(e, "ad-hoc-run", "write")
		ws, err := nightly.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "shared-workflow"})
		e.Must(err)
		_, err = nightly.Run(e.Ctx, ws.ID, "sh", "-c", "echo nightly > report.txt")
		e.Must(err)
		_, err = adhoc.Run(e.Ctx, ws.ID, "sh", "-c", "rm report.txt")
		e.Must(err)
		// the ad-hoc run's key goes; its history stays
		e.Must(e.C.RevokeKey(e.Ctx, kh.ID))
		evs := auditOf(e, "")
		who := map[string]string{}
		for _, ev := range evs {
			if ev.Kind == "exec" && ev.WorkspaceID == ws.ID {
				switch {
				case strings.Contains(ev.Summary, "echo nightly"):
					who["write"] = ev.Key
				case strings.Contains(ev.Summary, "rm report.txt"):
					who["delete"] = ev.Key
				}
			}
		}
		Equal(e, "who wrote the report", who["write"], kn.ID)
		Equal(e, "who removed it", who["delete"], kh.ID)
	})
}
