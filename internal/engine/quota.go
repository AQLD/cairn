package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/AQLD/cairn/internal/failpoint"
	"github.com/AQLD/cairn/internal/store"
)

// Run-time quotas: how long a workspace's sandbox may run (commands or not:
// that is what costs). The engine counts each running workspace's time on a
// tick, from the machine's monotonic clock so a wall-clock step never bills
// time that did not pass. A workspace that reaches its quota is paused (a
// full save, as an idle pause), never killed: its processes and files wait.
// While over its quota it refuses commands, writes, saves and waking, with
// ErrQuota; reading its files still works. Raising the quota lets the next
// use wake it as it was.

// DefaultQuotaInterval is how often run time is counted.
const DefaultQuotaInterval = 15 * time.Second

// ErrQuota: the workspace has used its quota; raise it to go on.
var ErrQuota = errors.New("quota exceeded")

// Quota limits a workspace. Zero fields are unlimited.
type Quota struct {
	// RunSeconds is how long its sandbox may run in all.
	RunSeconds int64 `json:"run_seconds,omitempty"`
}

func (q *Quota) validate() error {
	if q != nil && q.RunSeconds < 0 {
		return fmt.Errorf("%w: quota cannot be negative", ErrInvalid)
	}
	return nil
}

// overQuota reports whether a workspace, or the key that made it, has used
// its run-time quota.
func (e *Engine) overQuota(w *store.Workspace) bool {
	if _, over := e.keyOver.Load(w.Owner); over && w.Owner != "" {
		return true
	}
	return ownOverQuota(w)
}

// ownOverQuota reports whether a workspace has used its own quota.
func ownOverQuota(w *store.Workspace) bool {
	spec, err := specOf(w)
	if err != nil || spec.Quota == nil || spec.Quota.RunSeconds <= 0 {
		return false
	}
	return w.RunTime >= time.Duration(spec.Quota.RunSeconds)*time.Second
}

func (e *Engine) quotaErr(w *store.Workspace) error {
	if !ownOverQuota(w) {
		return fmt.Errorf("%w: the key %s that made workspace %s has used its run-time quota; an admin can raise it (its work is saved)", ErrQuota, w.Owner, w.ID)
	}
	spec, _ := specOf(w)
	return fmt.Errorf("%w: workspace %s has run %s of its %ds; raise its quota to go on (its work is saved)",
		ErrQuota, w.ID, w.RunTime.Round(time.Second), spec.Quota.RunSeconds)
}

// SetQuota changes a workspace's quota (nil or zero fields: unlimited). A
// workspace paused at its quota wakes on next use once it is under it.
func (e *Engine) SetQuota(ctx context.Context, id string, q *Quota) (*Workspace, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	w, err := e.liveWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	spec, err := specOf(w)
	if err != nil {
		return nil, err
	}
	spec.Quota = q
	if q != nil && q.RunSeconds == 0 {
		spec.Quota = nil
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	if err := e.store.SetSpec(ctx, id, raw); err != nil {
		return nil, err
	}
	e.note(ctx, id, "quota", fmt.Sprintf("quota set: %+v", q), nil, map[string]any{"quota": q})
	return e.GetWorkspace(ctx, id)
}

// refreshKeyQuotas notes which keys' workspaces have used the key's quota
// together.
func (e *Engine) refreshKeyQuotas(ctx context.Context) {
	keys, err := e.store.ListAPIKeys(ctx)
	if err != nil {
		return
	}
	var used map[string]time.Duration
	for _, k := range keys {
		if k.RunQuota <= 0 {
			e.keyOver.Delete(k.ID)
			continue
		}
		if used == nil {
			if used, err = e.store.RunTimeByOwner(ctx); err != nil {
				return
			}
		}
		if used[k.ID] >= time.Duration(k.RunQuota)*time.Second {
			e.keyOver.Store(k.ID, true)
		} else {
			e.keyOver.Delete(k.ID)
		}
	}
}

// checkKeyQuota refuses new work (a create or a copy) for a key over its
// quota.
func (e *Engine) checkKeyQuota(ctx context.Context) error {
	if k := owner(ctx); k != "" {
		if _, over := e.keyOver.Load(k); over {
			return fmt.Errorf("%w: key %s has used its run-time quota; an admin can raise it", ErrQuota, k)
		}
	}
	return nil
}

// SetKeyQuota sets a key's run-time quota, for its workspaces together (nil
// or zero: none). Its paused workspaces wake on next use once under it.
func (e *Engine) SetKeyQuota(ctx context.Context, id string, q *Quota) (Key, error) {
	if err := q.validate(); err != nil {
		return Key{}, err
	}
	var s int64
	if q != nil {
		s = q.RunSeconds
	}
	if err := e.store.SetKeyQuota(ctx, id, s); err != nil {
		return Key{}, err
	}
	e.refreshKeyQuotas(ctx)
	k, err := e.store.GetAPIKey(ctx, id)
	if err != nil {
		return Key{}, err
	}
	e.note(ctx, "", "key_quota", fmt.Sprintf("key %s quota set: %ds", id, s), nil, map[string]any{"quota_key": id, "run_seconds": s})
	return e.keyWithUsage(ctx, k), nil
}

// keyWithUsage is a key as the API shows it, with its workspaces' run time.
func (e *Engine) keyWithUsage(ctx context.Context, k store.APIKey) Key {
	out := publicKey(k)
	if used, err := e.store.RunTimeByOwner(ctx); err == nil {
		out.RunSeconds = used[k.ID].Seconds()
	}
	return out
}

// quotaLoop counts running workspaces' time and pauses those over quota,
// until ctx ends.
func (e *Engine) quotaLoop(ctx context.Context) {
	defer close(e.quotaDone)
	tick := time.NewTicker(e.cfg.QuotaInterval)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		e.countRunTime(ctx, now.Sub(last)) // monotonic: never a wall-clock step
		last = now
	}
}

func (e *Engine) countRunTime(ctx context.Context, elapsed time.Duration) {
	list, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return
	}
	for i := range list {
		w := &list[i]
		if w.State != store.Running {
			continue
		}
		if err := e.store.AddRunTime(ctx, w.ID, elapsed); err != nil {
			continue
		}
		list[i].RunTime += elapsed
	}
	e.refreshKeyQuotas(ctx)
	for i := range list {
		if list[i].State == store.Running && e.overQuota(&list[i]) {
			e.enforceQuota(ctx, list[i].ID)
		}
	}
}

// enforceQuota pauses a workspace that reached its quota. A command still
// running keeps it from pausing (new ones are refused already); the next tick
// tries again.
func (e *Engine) enforceQuota(ctx context.Context, id string) {
	if !e.cfg.Runtime.Capabilities().FullSave {
		return // nothing to save processes with: refusals alone stop new work
	}
	l := e.lock(id)
	if !l.TryLock() {
		return
	}
	defer l.Unlock()
	err := e.pauseLocked(ctx, id, "quota")
	switch {
	case err == nil:
		e.log.Info("workspace paused at its run-time quota", "workspace", id)
	case failpoint.Crashed(err):
	default:
		e.log.Error("could not pause a workspace at its quota", "workspace", id, "err", err)
	}
}
