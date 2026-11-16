package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AQLD/cairn/internal/redact"
	"github.com/AQLD/cairn/internal/save"
)

// The public operations: each runs its unexported twin and records the
// outcome in the workspace's activity log (activity.go).

func (e *Engine) CreateWorkspace(ctx context.Context, req CreateRequest) (*Workspace, error) {
	start := time.Now().Add(-time.Second) // creation times are the engine's clock, rounded
	w, err := e.createWorkspace(ctx, req)
	if err == nil && !w.CreatedAt.Before(start) { // a retried request returns the first one's workspace: already logged
		summary := "created"
		if w.Template != "" {
			summary += " from template " + w.Template
		}
		e.note(ctx, w.ID, "create", summary, nil, map[string]any{"name": w.Name, "template": w.Template,
			"network": w.Network, "limits": w.Limits})
	}
	return w, err
}

func (e *Engine) DeleteWorkspace(ctx context.Context, id string) error {
	err := e.deleteWorkspace(ctx, id)
	e.note(ctx, id, "delete", "deleted", err, nil)
	if err == nil {
		// keep the log for the retention window, like the workspace's saves
		if lerr := e.events.Ended(ctx, id, e.cfg.Now()); lerr != nil {
			e.log.Error("activity log: record deletion", "workspace", id, "err", lerr)
		}
	}
	return err
}

// WriteFile writes a file inside the workspace, creating parent folders.
func (e *Engine) WriteFile(ctx context.Context, id, name string, r io.Reader, mode os.FileMode) error {
	cr := &countingReader{r: r}
	err := e.writeFile(ctx, id, name, cr, mode)
	data := map[string]any{"path": name, "bytes": cr.n}
	if mode != 0 {
		data["mode"] = fmt.Sprintf("%04o", mode.Perm())
	}
	e.note(ctx, id, "file_write", fmt.Sprintf("wrote %s (%s)", name, size(cr.n)), err, data)
	return err
}

// OpenFile opens a file inside the workspace for reading. The caller closes it.
func (e *Engine) OpenFile(ctx context.Context, id, name string) (*os.File, os.FileInfo, error) {
	f, info, err := e.openFile(ctx, id, name)
	data := map[string]any{"path": name}
	summary := "read " + name
	if err == nil {
		data["bytes"] = info.Size()
		summary += " (" + size(info.Size()) + ")"
	}
	e.note(ctx, id, "file_read", summary, err, data)
	return f, info, err
}

// SaveWith is Save with options. Every save, and every failed one, goes into
// the workspace's activity log.
func (e *Engine) SaveWith(ctx context.Context, id string, opts SaveOptions) (*Save, error) {
	start := time.Now()
	kind := opts.Kind
	sv, err := e.saveWith(ctx, id, opts)
	if err != nil {
		e.note(ctx, id, "save", fmt.Sprintf("%s save", orQuick(kind)), err, map[string]any{"kind": orQuick(kind)})
		return sv, err
	}
	if sv.CreatedAt.Before(start.Add(-time.Second)) {
		return sv, nil // a retry answered with the first request's save
	}
	took := time.Since(start)
	data := map[string]any{"save": sv.ID, "kind": sv.Kind, "bytes": sv.Size, "duration_ms": took.Milliseconds()}
	if sv.Parent != "" {
		data["parent"] = sv.Parent
	}
	summary := fmt.Sprintf("%s save %s (%s) in %s", sv.Kind, sv.ID, size(sv.Size), seconds(took))
	if opts.KeepRunning && sv.Kind == save.Full {
		data["keep_running"] = true
		summary += ", kept running"
	}
	e.note(ctx, id, "save", summary, nil, data)
	return sv, nil
}

func orQuick(k save.Kind) save.Kind {
	if k == "" {
		return save.Quick
	}
	return k
}

// Resume restores a workspace to one of its saves: the filesystem, and for a
// full save its memory and processes. Anything written since the save is lost.
// The log keeps what happened before: it shows the rewind, it is not rewound.
func (e *Engine) Resume(ctx context.Context, saveID string) (*Workspace, error) {
	var id, from string
	var kind string
	if sv, err := e.store.GetSave(ctx, saveID); err == nil {
		id, kind = sv.WorkspaceID, sv.Kind
		if w, err := e.store.GetWorkspace(ctx, id); err == nil {
			from = w.BaseSave
		}
	}
	start := time.Now()
	w, err := e.resume(ctx, saveID)
	if id == "" {
		return w, err // no such save: nothing to log against
	}
	data := map[string]any{"save": saveID, "kind": kind, "duration_ms": time.Since(start).Milliseconds()}
	summary := fmt.Sprintf("resumed %s save %s", kind, saveID)
	if from != "" && from != saveID {
		data["from"] = from
		summary += " (state was from " + from + ")"
	}
	e.note(ctx, id, "resume", summary, err, data)
	return w, err
}

// Copy makes count new workspaces from one save. Each copy has its own ID and
// identity; the original workspace is untouched. Each copy's log starts with
// where it came from; the original's log lists the copies.
//
// requestKey, if set, makes the request idempotent: copy i is created with key
// "<requestKey>#i", so a retry returns the copies already made and creates only
// the ones a crash prevented.
func (e *Engine) Copy(ctx context.Context, saveID string, count int, requestKey string) ([]Workspace, error) {
	start := time.Now().Add(-time.Second)
	src := ""
	if sv, err := e.store.GetSave(ctx, saveID); err == nil {
		src = sv.WorkspaceID
	}
	ws, err := e.copy(ctx, saveID, count, requestKey)
	var made []string
	for _, w := range ws {
		if w.CreatedAt.Before(start) {
			continue // made by an earlier try of this request
		}
		made = append(made, w.ID)
		e.note(ctx, w.ID, "create", fmt.Sprintf("created as a copy of save %s of %s", saveID, src), nil,
			map[string]any{"name": w.Name, "from_save": saveID, "from_workspace": src, "network": w.Network, "limits": w.Limits})
	}
	if src != "" && (len(made) > 0 || err != nil) {
		e.note(ctx, src, "copy", fmt.Sprintf("copied save %s into %d workspace(s): %s", saveID, len(made), strings.Join(made, ", ")), err,
			map[string]any{"save": saveID, "count": count, "workspaces": made})
	}
	return ws, err
}

// CreateSession starts a session in a running workspace. language is
// "python" (needs python3 in the workspace) or "shell" (bash, else sh).
func (e *Engine) CreateSession(ctx context.Context, id, language string) (*Session, error) {
	s, err := e.createSession(ctx, id, language)
	data := map[string]any{"language": language}
	summary := language + " session"
	if err == nil {
		data["session"], data["pid"] = s.ID, s.PID
		summary = fmt.Sprintf("started %s session %s", language, s.ID)
	}
	e.note(ctx, id, "session_start", summary, err, data)
	return s, err
}

// RunInSession runs one cell. One cell runs at a time per session: a second
// call while one runs is a conflict.
func (e *Engine) RunInSession(ctx context.Context, id, sid string, run SessionRun) (SessionResult, error) {
	var stdout, stderr tail
	code := run.Code
	run.Stdout, run.Stderr = teeTo(run.Stdout, &stdout), teeTo(run.Stderr, &stderr)
	start := time.Now()
	res, err := e.runInSession(ctx, id, sid, run)
	took := time.Since(start)
	code = redact.String(code) // before it is cut short
	data := map[string]any{"session": sid, "code": clip(code, tailBytes), "duration_ms": took.Milliseconds(),
		"stdout_bytes": stdout.n, "stderr_bytes": stderr.n}
	summary := "session " + sid + ": " + oneLine(code)
	if err == nil {
		data["exit_code"], data["timed_out"], data["ended"] = res.ExitCode, res.TimedOut, res.Ended
		data["stdout_tail"], data["stderr_tail"] = stdout.String(), stderr.String()
		switch {
		case res.Ended:
			summary += " → the session ended"
		default:
			summary += " → " + ended(res.ExitCode, res.TimedOut)
		}
		summary += " in " + seconds(took)
	}
	e.note(ctx, id, "session_run", summary, err, data)
	return res, err
}

// DeleteSession stops a session and everything it started.
func (e *Engine) DeleteSession(ctx context.Context, id, sid string) error {
	err := e.deleteSession(ctx, id, sid)
	e.note(ctx, id, "session_stop", "stopped session "+sid, err, map[string]any{"session": sid})
	return err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func size(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}
