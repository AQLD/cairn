package engine

// The warm pool: workspaces created and started ahead of demand, so a create
// that matches one returns at once instead of building a filesystem and
// booting a sandbox. A warm workspace is an ordinary workspace that belongs
// to no one yet (state warm; warming while it is built): never listed, never
// reachable by ID, never paused. A create whose spec is exactly a warm
// workspace's claims it in one atomic update; it keeps its own ID, hostname,
// network and (empty) history, so nothing tells it from a cold create. The
// pool refills in the background.
//
// Warm workspaces never outlive the engine: opening it discards any left
// from before (a crash, a restart) and builds fresh ones. Replacing or
// deleting a template discards its warm workspaces, and a warm workspace
// that was being built from the old template is discarded when it finishes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/AQLD/cairn/internal/store"
)

// warmState is the pool's bookkeeping.
type warmState struct {
	gen    map[string]int // per template: bumped when it changes; a build from an older generation is discarded
	kick   chan struct{}
	stop   context.CancelFunc
	done   chan struct{}
	failed map[string]string // per template: last error, logged once
}

// warmSpec is the spec of a warm workspace for template: what a create with
// no limits or network asks for (its disk filled in as for any create).
func (e *Engine) warmSpec(template string) Spec {
	spec := Spec{Template: template}
	e.diskFor(&spec)
	return spec
}

// warmTemplate resolves a pool's key ("" is the default template).
func (e *Engine) warmTemplate(key string) string {
	if key == "" {
		return e.cfg.DefaultTemplate
	}
	return key
}

// startWarm discards warm workspaces left from before and, with a pool
// configured, starts filling it.
func (e *Engine) startWarm(ctx context.Context) error {
	e.warm.gen, e.warm.failed = map[string]int{}, map[string]string{}
	if err := e.discardWarm(ctx, func(Spec) bool { return true }); err != nil {
		return err
	}
	if len(e.cfg.Warm) == 0 {
		return nil
	}
	wctx, cancel := context.WithCancel(context.Background())
	e.warm.kick, e.warm.stop, e.warm.done = make(chan struct{}, 1), cancel, make(chan struct{})
	go e.warmLoop(wctx)
	e.kickWarm()
	return nil
}

func (e *Engine) stopWarm() {
	if e.warm.stop != nil {
		e.warm.stop()
		<-e.warm.done
	}
}

func (e *Engine) kickWarm() {
	if e.warm.kick == nil {
		return
	}
	select {
	case e.warm.kick <- struct{}{}:
	default:
	}
}

func (e *Engine) warmLoop(ctx context.Context) {
	defer close(e.warm.done)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		e.fillWarm(ctx)
		select {
		case <-ctx.Done():
			return
		case <-e.warm.kick:
		case <-tick.C:
		}
	}
}

// fillWarm builds warm workspaces until every pool is at its size.
func (e *Engine) fillWarm(ctx context.Context) {
	for key, size := range e.cfg.Warm {
		template := e.warmTemplate(key)
		if template == "" || size <= 0 {
			continue
		}
		for ctx.Err() == nil {
			n, err := e.warmCount(ctx, template)
			if err != nil || n >= size {
				break
			}
			if err := e.makeWarm(ctx, template); err != nil {
				e.warmFailed(template, err)
				break
			}
			delete(e.warm.failed, template)
		}
	}
}

func (e *Engine) warmFailed(template string, err error) {
	if e.warm.failed[template] != err.Error() {
		e.log.Warn("could not fill the warm pool", "template", template, "err", err)
		e.warm.failed[template] = err.Error()
	}
}

// warmCount is how many warm (and warming) workspaces template has.
func (e *Engine) warmCount(ctx context.Context, template string) (int, error) {
	ws, err := e.store.WarmWorkspaces(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range ws {
		if s, err := specOf(&ws[i]); err == nil && s.Template == template {
			n++
		}
	}
	return n, nil
}

// WarmIDs lists template's warm workspaces, ready to be claimed (not the
// ones being built).
func (e *Engine) WarmIDs(ctx context.Context, template string) ([]string, error) {
	ws, err := e.store.WarmWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for i := range ws {
		if s, err := specOf(&ws[i]); err == nil && s.Template == template && ws[i].State == store.Warm {
			ids = append(ids, ws[i].ID)
		}
	}
	return ids, nil
}

// makeWarm builds and starts one warm workspace of template. Recorded
// (warming) before anything is built, so a crash leaves a row that opening
// the engine discards, never a half-built workspace someone could get.
func (e *Engine) makeWarm(ctx context.Context, template string) error {
	if _, err := e.GetTemplate(ctx, template); err != nil {
		return err
	}
	if err := e.needSpace("a warm workspace", 0); err != nil {
		return err
	}
	release, err := e.heavy(ctx)
	if err != nil {
		return err
	}
	defer release()
	gen := e.warmGen(template)
	spec := e.warmSpec(template)
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	id := newID("ws")
	now := time.Now().UTC()
	w := store.Workspace{ID: id, Name: id, State: store.Warming, Spec: raw, CreatedAt: now, UpdatedAt: now}
	if err := e.store.CreateWorkspace(ctx, w); err != nil {
		return err
	}
	discard := func() { _ = e.dropWarmOne(context.Background(), &w) }
	if err := os.MkdirAll(e.workspaceDir(id), 0o700); err != nil {
		discard()
		return err
	}
	if err := e.createRootfs(id, spec); err != nil {
		discard()
		return noSpace(err)
	}
	rs, err := e.runtimeSpec(&w)
	if err == nil {
		err = e.cfg.Runtime.Start(ctx, rs)
	}
	if err != nil {
		discard()
		return err
	}
	if e.warmGen(template) != gen { // the template changed while this was built
		discard()
		return nil
	}
	return e.store.SetWorkspaceState(ctx, id, store.Warm)
}

func (e *Engine) warmGen(template string) int {
	e.warmMu.Lock()
	defer e.warmMu.Unlock()
	return e.warm.gen[template]
}

// claimWarm hands a warm workspace with exactly spec to a create, or returns
// nil if none is warm.
func (e *Engine) claimWarm(ctx context.Context, spec Spec, name, requestKey string) (*store.Workspace, error) {
	if len(e.cfg.Warm) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	w, err := e.store.ClaimWarm(ctx, raw, name, requestKey, time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.kickWarm()
	if err := e.writeRecord(w); err != nil {
		e.log.Warn("could not write the record of a workspace from the warm pool", "workspace", w.ID, "err", err)
	}
	return w, nil
}

// dropWarm discards template's warm workspaces (its content changed) and
// any being built; the pool refills from the new template.
func (e *Engine) dropWarm(ctx context.Context, template string) {
	e.warmMu.Lock()
	if e.warm.gen != nil {
		e.warm.gen[template]++
	}
	e.warmMu.Unlock()
	if err := e.discardWarm(ctx, func(s Spec) bool { return s.Template == template }); err != nil {
		e.log.Warn("could not discard warm workspaces", "template", template, "err", err)
	}
	e.kickWarm()
}

// discardWarm stops and removes the warm workspaces whose spec matches. A
// warming one being built right now is left to its builder (its generation
// check discards it); at startup there are none.
func (e *Engine) discardWarm(ctx context.Context, match func(Spec) bool) error {
	ws, err := e.store.WarmWorkspaces(ctx)
	if err != nil {
		return err
	}
	for i := range ws {
		s, err := specOf(&ws[i])
		if err != nil || !match(s) {
			continue
		}
		if ws[i].State == store.Warming && e.warm.done != nil {
			continue
		}
		if err := e.dropWarmOne(ctx, &ws[i]); err != nil {
			return err
		}
	}
	return nil
}

// dropWarmOne stops and removes one warm workspace. Recorded deleted first:
// a crash after that leaves files that removal of deleted workspaces cleans.
func (e *Engine) dropWarmOne(ctx context.Context, w *store.Workspace) error {
	if err := e.store.SetWorkspaceState(ctx, w.ID, store.Deleted); err != nil {
		return err
	}
	if rs, err := e.runtimeSpec(w); err == nil {
		if err := e.cfg.Runtime.Stop(ctx, rs); err != nil {
			e.log.Warn("could not stop a warm workspace", "workspace", w.ID, "err", err)
		}
	}
	spec, err := specOf(w)
	if err != nil {
		return err
	}
	if err := e.removeWorkspaceDir(w.ID, spec); err != nil {
		return fmt.Errorf("remove warm workspace %s: %w", w.ID, err)
	}
	return nil
}
