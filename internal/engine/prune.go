package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/AQLD/cairn/internal/store"
)

// DefaultKeepSaves and DefaultKeepSavesFor are the pruning policy: per
// workspace, the newest DefaultKeepSaves saves and every save younger than
// DefaultKeepSavesFor are kept; older ones beyond that are deleted (and stay
// recoverable for the retention window, like any deleted save).
const (
	DefaultKeepSaves    = 20
	DefaultKeepSavesFor = 7 * 24 * time.Hour
)

// pruneOff is the value of a pruning limit that is turned off.
const pruneOff = -1

// Prune deletes the saves the pruning policy no longer keeps and returns how
// many. A save is kept when it is among its workspace's newest KeepSaves, or
// younger than KeepSavesFor, or pinned, or a pause save, or the save a
// workspace's state stands on (its base or pause save, whichever workspace
// it belongs to). Only ready saves are pruned; with both limits off nothing
// is. Pruning is a soft delete: the save can be undeleted until purged.
func (e *Engine) Prune(ctx context.Context) (int, error) {
	keepN, keepFor := e.cfg.KeepSaves, e.cfg.KeepSavesFor
	if keepN == pruneOff && keepFor == pruneOff {
		return 0, nil
	}
	wss, err := e.store.ListWorkspaces(ctx)
	if err != nil {
		return 0, err
	}
	standsOn := map[string]bool{}
	for _, w := range wss {
		standsOn[w.BaseSave] = true
		standsOn[w.PausedSave] = true
	}
	all, err := e.store.AllSaves(ctx)
	if err != nil {
		return 0, err
	}
	byWorkspace := map[string][]store.Save{}
	for _, sv := range all {
		if sv.State == store.SaveReady && !sv.Auto {
			byWorkspace[sv.WorkspaceID] = append(byWorkspace[sv.WorkspaceID], sv)
		}
	}
	now := e.cfg.Now()
	n := 0
	for ws, svs := range byWorkspace {
		sort.Slice(svs, func(i, j int) bool {
			if !svs[i].CreatedAt.Equal(svs[j].CreatedAt) {
				return svs[i].CreatedAt.After(svs[j].CreatedAt)
			}
			return svs[i].ID > svs[j].ID
		})
		for i, sv := range svs {
			switch {
			case keepN != pruneOff && i < keepN,
				keepFor != pruneOff && now.Sub(sv.CreatedAt) < keepFor,
				sv.Pinned, standsOn[sv.ID]:
				continue
			}
			if err := e.store.SoftDeleteSave(ctx, sv.ID, now); err != nil {
				return n, err
			}
			n++
			e.log.Info("pruned save", "save", sv.ID, "workspace", ws, "created_at", sv.CreatedAt)
			e.note(ctx, ws, "prune", fmt.Sprintf("pruned save %s", sv.ID), nil, map[string]any{"save": sv.ID})
		}
	}
	return n, nil
}
