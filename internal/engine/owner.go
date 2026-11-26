package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/internal/objstore"
	"github.com/AQLD/cairn/internal/store"
)

// Owners: which engine runs a workspace, for engines sharing a bucket. Each
// engine has an ID (data/engine-id). With object storage that can write
// conditionally, every live workspace has an owner record
//
//	workspaces/<id>/owner.json   {engine, epoch, renewed_at, ttl_s}
//
// that the engine running it renews every third of its lease. Another engine
// adopts the workspace (resume with adopt) only once that lease has run out,
// and takes it over with a conditional write, so of two engines racing,
// exactly one wins; each takeover raises the epoch. Leases compare one
// engine's renewed_at with another's clock: servers' clocks must agree to
// well within a lease (NTP does).
//
// Without object storage, or with one that cannot write conditionally, there
// are no owner records and adopt trusts the caller (G7.17).

// DefaultLeaseTTL is how long an owner record holds without renewal.
const DefaultLeaseTTL = 2 * time.Minute

type ownerRecord struct {
	Engine    string    `json:"engine"`
	Epoch     int       `json:"epoch"`
	RenewedAt time.Time `json:"renewed_at"`
	TTLSec    float64   `json:"ttl_s"`
}

func (o ownerRecord) expires() time.Time {
	return o.RenewedAt.Add(time.Duration(o.TTLSec * float64(time.Second)))
}

func ownerKey(ws string) string { return "workspaces/" + ws + "/owner.json" }

// leases returns the store owner records live in, or nil when there are none.
func (e *Engine) leases() objstore.Conditional {
	if !e.remoteOn() {
		return nil
	}
	c, _ := e.cfg.Objects.(objstore.Conditional)
	return c
}

// loadEngineID reads this engine's ID, making one on first start.
func loadEngineID(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "engine-id")
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	id := "eng_" + hex.EncodeToString(r[:])
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := syncFile(tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return id, syncDir(dataDir)
}

func (e *Engine) readOwner(ctx context.Context, c objstore.Conditional, ws string) (*ownerRecord, string, error) {
	data, version, err := c.GetVersion(ctx, ownerKey(ws))
	if errors.Is(err, objstore.ErrNotFound) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var o ownerRecord
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, "", fmt.Errorf("owner record of %s: %w", ws, err)
	}
	return &o, version, nil
}

func (e *Engine) writeOwner(ctx context.Context, c objstore.Conditional, ws string, epoch int, ifMatch string) error {
	b, err := json.Marshal(ownerRecord{Engine: e.engineID, Epoch: epoch, RenewedAt: e.cfg.Now().UTC(), TTLSec: e.cfg.LeaseTTL.Seconds()})
	if err != nil {
		return err
	}
	_, err = c.PutIf(ctx, ownerKey(ws), b, ifMatch)
	return err
}

// takeOwner makes this engine the owner of ws before it adopts it: refused
// while another engine's lease holds, and when another engine wins a race
// for it.
func (e *Engine) takeOwner(ctx context.Context, ws string) error {
	c := e.leases()
	if c == nil {
		return nil
	}
	o, version, err := e.readOwner(ctx, c, ws)
	if err != nil {
		return fmt.Errorf("reading the owner of workspace %s: %w", ws, err)
	}
	epoch := 1
	if o != nil {
		if o.Engine != e.engineID && e.cfg.Now().Before(o.expires()) {
			return fmt.Errorf("%w: workspace %s is run by engine %s (its lease holds until %s, %s from now); adopt it once that server is gone and the lease has run out",
				ErrConflict, ws, o.Engine, o.expires().UTC().Format(time.RFC3339), o.expires().Sub(e.cfg.Now()).Round(time.Second))
		}
		epoch = o.Epoch + 1
	}
	err = e.writeOwner(ctx, c, ws, epoch, version)
	if errors.Is(err, objstore.ErrPrecondition) {
		return fmt.Errorf("%w: another engine took workspace %s first", ErrConflict, ws)
	}
	if err != nil {
		// unknown outcome: it is ours only if the record now says so
		if o2, _, rerr := e.readOwner(ctx, c, ws); rerr == nil && o2 != nil && o2.Engine == e.engineID && o2.Epoch == epoch {
			return nil
		}
		return fmt.Errorf("claiming workspace %s: %w", ws, err)
	}
	e.log.Info("workspace owner record taken", "workspace", ws, "epoch", epoch)
	return nil
}

// renewOwners claims or renews the owner record of every live workspace
// here. A workspace another engine now owns is left alone (and logged): it
// was adopted elsewhere while this engine was cut off.
func (e *Engine) renewOwners(ctx context.Context) {
	c := e.leases()
	if c == nil {
		return
	}
	ws, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return
	}
	for _, w := range ws {
		if ctx.Err() != nil {
			return
		}
		if w.State == store.Moved {
			continue
		}
		o, version, err := e.readOwner(ctx, c, w.ID)
		switch {
		case err != nil:
			e.log.Warn("could not read a workspace's owner record; will retry", "workspace", w.ID, "err", err)
			continue
		case o == nil:
			err = e.writeOwner(ctx, c, w.ID, 1, "")
		case o.Engine == e.engineID:
			err = e.writeOwner(ctx, c, w.ID, o.Epoch, version)
		default:
			e.fence(ctx, w.ID, o)
			continue
		}
		if err != nil && !errors.Is(err, objstore.ErrPrecondition) {
			e.log.Warn("could not renew a workspace's owner record; will retry", "workspace", w.ID, "err", err)
		}
	}
}

// fence stops a workspace another engine now owns (it adopted it while this
// one was cut off from the bucket) and marks it moved: nothing runs it here
// again, and its saves are not uploaded (upload).
func (e *Engine) fence(ctx context.Context, id string, o *ownerRecord) {
	e.movedTo.Store(id, o.Engine)
	l := e.lock(id)
	l.cancelCommands()
	l.Lock()
	defer l.Unlock()
	w, err := e.store.GetWorkspace(ctx, id)
	if err != nil || w.State == store.Moved || w.State.Hidden() {
		return
	}
	if rs, err := e.runtimeSpec(w); err == nil {
		if err := e.cfg.Runtime.Stop(ctx, rs); err != nil {
			e.log.Error("could not stop a workspace another engine took over", "workspace", id, "err", err)
		}
	}
	if err := e.store.SetWorkspaceState(ctx, id, store.Moved); err != nil {
		e.log.Error("could not mark a workspace moved", "workspace", id, "err", err)
		return
	}
	e.log.Error("workspace adopted by another engine: stopped here", "workspace", id, "owner", o.Engine, "epoch", o.Epoch)
	e.note(ctx, id, "moved", fmt.Sprintf("adopted by engine %s; stopped here", o.Engine), nil, map[string]any{"engine": o.Engine, "epoch": o.Epoch})
}

// movedErr is the refusal for a workspace another engine took over.
func (e *Engine) movedErr(id string) error {
	if to, ok := e.movedTo.Load(id); ok {
		return fmt.Errorf("%w: workspace %s was adopted by engine %s and runs there now; it can only be deleted here", ErrConflict, id, to)
	}
	return fmt.Errorf("%w: workspace %s was adopted by another engine and runs there now; it can only be deleted here", ErrConflict, id)
}

// mayUpload says whether this engine may still upload a save of ws: not once
// another engine owns it (fenced, or found so now).
func (e *Engine) mayUpload(ctx context.Context, ws string) (bool, error) {
	if w, err := e.store.GetWorkspace(ctx, ws); err == nil && w.State == store.Moved {
		return false, nil
	}
	c := e.leases()
	if c == nil {
		return true, nil
	}
	o, _, err := e.readOwner(ctx, c, ws)
	if err != nil {
		return false, err
	}
	if o != nil && o.Engine != e.engineID {
		e.fence(ctx, ws, o)
		return false, nil
	}
	return true, nil
}

// releaseOwner removes a deleted workspace's owner record, if it is this
// engine's. Best effort: an orphaned record just runs out.
func (e *Engine) releaseOwner(ws string) {
	c := e.leases()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if o, _, err := e.readOwner(ctx, c, ws); err == nil && o != nil && o.Engine == e.engineID {
		_ = e.cfg.Objects.Delete(ctx, ownerKey(ws))
	}
}

// ownerLoop renews owner records every third of a lease until ctx ends.
func (e *Engine) ownerLoop(ctx context.Context) {
	defer close(e.ownerDone)
	tick := time.NewTicker(e.cfg.LeaseTTL / 3)
	defer tick.Stop()
	for {
		e.renewOwners(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
