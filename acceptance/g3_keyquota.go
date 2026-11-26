package acceptance

import (
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
)

// quotaKey makes an own write key with a run-time quota of seconds, on an
// engine with tokens that counts run time every 200 ms.
func quotaKey(e *Env, name string, seconds int64) (*client.Client, *client.Key) {
	e.T.Helper()
	k, err := e.C.CreateOwnKey(e.Ctx, name, "write")
	e.Must(err)
	if seconds > 0 {
		got, err := e.C.SetKeyQuota(e.Ctx, k.ID, client.Quota{RunSeconds: seconds})
		e.Must(err)
		if got.Quota == nil || got.Quota.RunSeconds != seconds || got.Token != "" {
			e.T.Fatalf("key after setting its quota: %+v", got)
		}
	}
	url, _ := e.endpoint()
	return client.New(url, k.Token), k
}

func withQuotaAuth(e *Env, configure func(*engine.Config)) {
	e.T.Helper()
	e.configure = func(c *engine.Config) {
		c.QuotaInterval = 200 * time.Millisecond
		if configure != nil {
			configure(c)
		}
	}
	withAuth(e)
}

// waitRefused waits until a command in ws is refused for its quota.
func waitRefused(e *Env, c *client.Client, ws string) {
	e.T.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := c.Exec(e.Ctx, ws, client.Exec{Command: []string{"true"}})
		if client.IsCode(err, "quota_exceeded") {
			return
		}
		if time.Now().After(deadline) {
			e.T.Fatalf("workspace %s still takes commands past its key's quota (last: %v)", ws, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func init() {
	register("G3.4.1", func(e *Env) {
		withQuotaAuth(e, nil)
		a, _ := quotaKey(e, "user-a", 3)
		b, _ := quotaKey(e, "user-b", 0)
		w1, err := a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "a1"})
		e.Must(err)
		w2, err := a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "a2"})
		e.Must(err)
		e.Must(a.WriteFile(e.Ctx, w1.ID, "work.txt", []byte("a's")))
		wb, err := b.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "b1"})
		e.Must(err)
		// two workspaces together reach 3 s in about 1.5 s
		waitRefused(e, a, w1.ID)
		waitRefused(e, a, w2.ID)
		got, err := a.ReadFile(e.Ctx, w1.ID, "work.txt")
		e.Must(err)
		Equal(e, "a's files stay readable", string(got), "a's")
		if e.Info.Capabilities.FullSave {
			for _, id := range []string{w1.ID, w2.ID} {
				deadline := time.Now().Add(10 * time.Second)
				for {
					ws, err := a.GetWorkspace(e.Ctx, id)
					e.Must(err)
					if ws.State == "paused" {
						break
					}
					if time.Now().After(deadline) {
						e.T.Fatalf("workspace %s of a key over its quota is %s, want paused", id, ws.State)
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
		_, err = b.Exec(e.Ctx, wb.ID, client.Exec{Command: []string{"true"}})
		e.Must(err) // another key's work runs on
	})

	register("G3.4.2", func(e *Env) {
		withQuotaAuth(e, nil)
		a, ka := quotaKey(e, "user-a", 2)
		w, err := a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "a1"})
		e.Must(err)
		waitRefused(e, a, w.ID)
		_, err = a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "a2"})
		e.ErrCode(err, "quota_exceeded")
		_, err = e.C.SetKeyQuota(e.Ctx, ka.ID, client.Quota{RunSeconds: 3600})
		e.Must(err)
		_, err = a.Exec(e.Ctx, w.ID, client.Exec{Command: []string{"true"}})
		e.Must(err)
		_, err = a.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "a2"})
		e.Must(err)
	})

	register("G3.4.3", func(e *Env) {
		e.RestartWith(func(c *engine.Config) { c.QuotaInterval = 200 * time.Millisecond; c.DefaultRunQuota = 2 * time.Second })
		dflt, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "default"})
		e.Must(err)
		if dflt.Quota == nil || dflt.Quota.RunSeconds != 2 {
			e.T.Fatalf("quota of a workspace created without one: %+v, want the engine's 2 s", dflt.Quota)
		}
		own, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "own", Quota: &client.Quota{RunSeconds: 3600}})
		e.Must(err)
		waitRefused(e, e.C, dflt.ID)
		_, err = e.C.Exec(e.Ctx, own.ID, client.Exec{Command: []string{"true"}})
		e.Must(err) // its own quota replaces the default
	})
}
