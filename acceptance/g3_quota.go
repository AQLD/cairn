package acceptance

import (
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
)

// quotaWorkspace makes a workspace with a run-time quota of seconds, on an
// engine that counts run time every 200 ms.
func quotaWorkspace(e *Env, seconds int64) *client.Workspace {
	e.T.Helper()
	e.RestartWith(func(c *engine.Config) { c.QuotaInterval = 200 * time.Millisecond })
	ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "quota", Quota: &client.Quota{RunSeconds: seconds}})
	e.Must(err)
	return ws
}

// waitOverQuota waits until the workspace has run past its quota and (where
// processes can be saved) the engine has paused it.
func waitOverQuota(e *Env, id string) *client.Workspace {
	e.T.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		ws, err := e.C.GetWorkspace(e.Ctx, id)
		e.Must(err)
		over := ws.Quota != nil && ws.RunSeconds >= float64(ws.Quota.RunSeconds)
		if over && (ws.State == "paused" || !e.Info.Capabilities.FullSave) {
			return ws
		}
		if time.Now().After(deadline) {
			e.T.Fatalf("workspace not paused at its quota: %+v", ws)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func init() {
	register("G3.3.1", func(e *Env) {
		ws := quotaWorkspace(e, 3)
		e.Write(ws.ID, "work.txt", "half done")
		background(e, ws.ID, counterProgram)
		got := waitOverQuota(e, ws.ID)
		Equal(e, "state at the quota", got.State, "paused")
		Equal(e, "files after the quota", e.Read(ws.ID, "work.txt"), "half done")
	})

	register("G3.3.2", func(e *Env) {
		ws := quotaWorkspace(e, 2)
		e.Write(ws.ID, "work.txt", "half done")
		waitOverQuota(e, ws.ID)
		_, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"echo", "hi"}})
		e.ErrCode(err, "quota_exceeded")
		if !strings.Contains(err.Error(), "raise") {
			e.T.Fatalf("refusal %q: want it to say how to go on", err)
		}
		e.ErrCode(e.C.WriteFile(e.Ctx, ws.ID, "more.txt", []byte("x")), "quota_exceeded")
		_, err = e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		e.ErrCode(err, "quota_exceeded")
		Equal(e, "files stay readable", e.Read(ws.ID, "work.txt"), "half done")
	})

	register("G3.3.3", func(e *Env) {
		ws := quotaWorkspace(e, 3)
		background(e, ws.ID, counterProgram)
		before := running(e, ws.ID)
		waitOverQuota(e, ws.ID)
		_, err := e.C.SetQuota(e.Ctx, ws.ID, client.Quota{RunSeconds: 3600})
		e.Must(err)
		e.Sh(ws.ID, "true") // the next command wakes it
		after := running(e, ws.ID)
		Equal(e, "the same process after raising the quota", after.PID, before.PID)
		if after.N <= before.N {
			e.T.Fatalf("counter %d -> %d: the process did not carry on", before.N, after.N)
		}
	})
}
