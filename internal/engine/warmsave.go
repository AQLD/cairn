package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/internal/save"
	"github.com/AQLD/cairn/internal/store"
)

// Warm saves: a template can have a warm-up command (start a language
// server, import heavy modules, open a database). The warm pool then runs it
// once, takes a full save of the result, and makes its warm workspaces copies
// of that save: a workspace from the pool arrives with the warm-up's
// processes already running, memory and all, and its own identity (a copy's
// hostname, address and randomness, G2.6).
//
// The warm-up and the save it made live in templates/.warm/<name>.json, out
// of the template's own folder (which a re-import replaces), so a new import
// keeps the warm-up and drops only the stale save. The save is the engine's:
// pinned against pruning, auto so callers cannot resume, copy or delete it.

// warmupTimeout bounds a warm-up command.
const warmupTimeout = 10 * time.Minute

type warmup struct {
	Command []string `json:"command"`
	Save    string   `json:"save,omitempty"` // the full save of the warmed-up workspace
}

func (e *Engine) warmupPath(template string) string {
	return filepath.Join(e.templatesDir(), ".warm", template+".json")
}

func (e *Engine) readWarmup(template string) (warmup, bool) {
	var w warmup
	data, err := os.ReadFile(e.warmupPath(template))
	if err != nil || json.Unmarshal(data, &w) != nil || len(w.Command) == 0 {
		return warmup{}, false
	}
	return w, true
}

func (e *Engine) writeWarmup(template string, w warmup) error {
	if err := os.MkdirAll(filepath.Dir(e.warmupPath(template)), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	tmp := e.warmupPath(template) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, e.warmupPath(template))
}

// SetTemplateWarmup sets (or, with no command, clears) a template's warm-up
// command. Its warm workspaces and warm save are discarded; the pool builds
// new ones.
func (e *Engine) SetTemplateWarmup(ctx context.Context, template string, command []string) (*Template, error) {
	t, err := e.GetTemplate(ctx, template)
	if err != nil {
		return nil, err
	}
	old, _ := e.readWarmup(template)
	if len(command) == 0 {
		if err := os.Remove(e.warmupPath(template)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else if err := e.writeWarmup(template, warmup{Command: command}); err != nil {
		return nil, err
	}
	e.dropWarmSave(ctx, old.Save)
	e.dropWarm(ctx, template)
	t.Warmup = command
	return t, nil
}

// templateChanged forgets a template's warm save (its content changed or it
// is gone); with gone, the warm-up too.
func (e *Engine) templateChanged(ctx context.Context, template string, gone bool) {
	w, ok := e.readWarmup(template)
	if !ok {
		return
	}
	e.dropWarmSave(ctx, w.Save)
	if gone {
		_ = os.Remove(e.warmupPath(template))
		return
	}
	w.Save = ""
	if err := e.writeWarmup(template, w); err != nil {
		e.log.Warn("could not reset a template's warm save", "template", template, "err", err)
	}
}

func (e *Engine) dropWarmSave(ctx context.Context, id string) {
	if id == "" {
		return
	}
	_ = e.store.SetPinned(ctx, id, false)
	if err := e.store.SoftDeleteSave(ctx, id, e.cfg.Now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.log.Warn("could not drop a warm save", "save", id, "err", err)
	}
}

// warmSaveFor returns template's warm save, building it if it has a warm-up
// and none yet; "" if the template has no warm-up.
func (e *Engine) warmSaveFor(ctx context.Context, template string) (string, error) {
	w, ok := e.readWarmup(template)
	if !ok {
		return "", nil
	}
	if w.Save != "" {
		if sv, err := e.store.GetSave(ctx, w.Save); err == nil && sv.State == store.SaveReady {
			return w.Save, nil
		}
	}
	gen := e.warmGen(template)
	id, err := e.buildWarmSave(ctx, template, w.Command)
	if err != nil {
		return "", err
	}
	if e.warmGen(template) != gen { // the template or its warm-up changed meanwhile
		e.dropWarmSave(ctx, id)
		return "", errors.New("template changed while warming up")
	}
	w.Save = id
	if err := e.writeWarmup(template, w); err != nil {
		e.dropWarmSave(ctx, id)
		return "", err
	}
	return id, nil
}

// buildWarmSave runs the warm-up in a hidden workspace of the template and
// returns a full save of it. The workspace goes; the save stays.
func (e *Engine) buildWarmSave(ctx context.Context, template string, command []string) (string, error) {
	ictx := internal(ctx)
	spec := e.warmSpec(template)
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	id := newID("ws")
	now := time.Now().UTC()
	w := store.Workspace{ID: id, Name: id, State: store.Warming, Spec: raw, CreatedAt: now, UpdatedAt: now}
	if err := e.store.CreateWorkspace(ctx, w); err != nil {
		return "", err
	}
	defer func() { _ = e.dropWarmOne(context.Background(), &w) }()
	if err := os.MkdirAll(e.workspaceDir(id), 0o700); err != nil {
		return "", err
	}
	if err := e.createRootfs(id, spec); err != nil {
		return "", noSpace(err)
	}
	rs, err := e.runtimeSpec(&w)
	if err != nil {
		return "", err
	}
	if err := e.cfg.Runtime.Start(ctx, rs); err != nil {
		return "", err
	}
	var out tail
	res, err := e.exec(ictx, id, ExecRequest{Argv: command, Stdout: &out, Stderr: &out, Timeout: warmupTimeout})
	if err != nil {
		return "", fmt.Errorf("warm-up: %w", err)
	}
	if res.ExitCode != 0 || res.TimedOut {
		return "", fmt.Errorf("warm-up %q ended with %s: %s", strings.Join(command, " "), ended(res.ExitCode, res.TimedOut), out.String())
	}
	sv, err := e.saveWith(ictx, id, SaveOptions{Kind: save.Full})
	if err != nil {
		return "", fmt.Errorf("warm save: %w", err)
	}
	if err := e.store.SetAuto(ctx, sv.ID); err != nil {
		return "", err
	}
	if err := e.store.SetPinned(ctx, sv.ID, true); err != nil {
		return "", err
	}
	e.log.Info("template warmed up", "template", template, "save", sv.ID, "bytes", sv.Size)
	return sv.ID, nil
}
