// Package client is the Go client for Cairn's REST API.
//
//	c := client.New("http://127.0.0.1:7420", "")
//	ws, _ := c.CreateWorkspace(ctx, client.CreateWorkspace{Name: "demo"})
//	out, _ := c.Exec(ctx, ws.ID, client.Exec{Command: []string{"echo", "hi"}})
//	sv, _ := c.Save(ctx, ws.ID, client.QuickSave)
//	_, _ = c.Resume(ctx, sv.ID)
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the engine at base, for example http://127.0.0.1:7420.
func New(base, token string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{}}
}

// WithHTTPClient replaces the underlying HTTP client (for timeouts, transports).
func (c *Client) WithHTTPClient(h *http.Client) *Client { c.http = h; return c }

// Error is an error returned by the engine.
type Error struct {
	Status    int
	Code      string // stable: not_found, invalid_argument, conflict, save_corrupt, insufficient_storage, too_many_requests, unsupported_by_runtime, unauthorized, internal
	Message   string
	RequestID string // matches the engine's log line for this request
}

func (e *Error) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("cairn: %s (%d): %s [request %s]", e.Code, e.Status, e.Message, e.RequestID)
	}
	return fmt.Sprintf("cairn: %s (%d): %s", e.Code, e.Status, e.Message)
}

type idempotencyKeyCtx struct{}

// WithIdempotencyKey returns a context that makes CreateWorkspace, Save and
// Copy safe to retry: send the same key again (after a timeout, a lost
// connection or an engine restart) and the engine returns the original result
// instead of doing the work twice. Use a fresh key per intended operation.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idempotencyKeyCtx{}, key)
}

// IsCode reports whether err is an engine error with the given code.
func IsCode(err error, code string) bool {
	if e, ok := err.(*Error); ok {
		return e.Code == code
	}
	return false
}

type Capabilities struct {
	Isolation           bool `json:"isolation"`
	FullSave            bool `json:"full_save"`
	ResourceLimits      bool `json:"resource_limits"` // CPU, memory, process count
	DiskLimits          bool `json:"disk_limits"`
	NetworkControl      bool `json:"network_control"`
	PersistentProcesses bool `json:"persistent_processes"`
}

type Info struct {
	Runtime        string       `json:"runtime"`
	RuntimeVersion string       `json:"runtime_version"`
	Capabilities   Capabilities `json:"capabilities"`
	SaveFormat     int          `json:"save_format"`
}

type Limits struct {
	CPUs     float64 `json:"cpus,omitempty"`
	MemoryMB int64   `json:"memory_mb,omitempty"`
	DiskMB   int64   `json:"disk_mb,omitempty"`
}

type CreateWorkspace struct {
	Name     string `json:"name,omitempty"`
	Limits   Limits `json:"limits,omitempty"`
	Network  bool   `json:"network,omitempty"`
	Template string `json:"template,omitempty"` // empty: the engine's default template
}

// Template is a root filesystem new workspaces start from.
type Template struct {
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	CreatedAt   time.Time `json:"created_at"`
	Image       string    `json:"image,omitempty"`        // the container image it was pulled from
	ImageDigest string    `json:"image_digest,omitempty"` // that image's manifest digest
}

// PullTemplate is a container image to build a template from.
type PullTemplate struct {
	Image string `json:"image"` // e.g. registry.internal/team/python:3.12
	// Registry login, used for this pull only (never stored by the engine).
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Insecure bool   `json:"insecure,omitempty"` // plain HTTP to the registry
}

type Workspace struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	State      string    `json:"state"` // running, stopped (full-saved), paused (woken on next use), ...
	Limits     Limits    `json:"limits"`
	Network    bool      `json:"network"`
	Template   string    `json:"template,omitempty"`
	SourceSave string    `json:"source_save,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type SaveKind string

const (
	QuickSave SaveKind = "quick"
	FullSave  SaveKind = "full"
)

type Save struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Kind        SaveKind   `json:"kind"`
	State       string     `json:"state"` // ready, missing, corrupt, deleted
	Size        int64      `json:"size"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
	PurgeAfter  *time.Time `json:"purge_after,omitempty"` // a deleted save can be undeleted until then
	Auto        bool       `json:"auto,omitempty"`        // made by the engine to pause the workspace
	Uploaded    bool       `json:"uploaded,omitempty"`    // a copy is in object storage
}

type Exec struct {
	Command   []string          `json:"command"`
	Env       map[string]string `json:"env,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Stdin     string            `json:"stdin,omitempty"`
	TimeoutMS int64             `json:"timeout_ms,omitempty"`
}

// ExecResult is how a command ended. Stdout and Stderr hold the exact bytes the
// command wrote (binary output included), up to 4 MiB each.
type ExecResult struct {
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	StdoutEncoding  string `json:"stdout_encoding,omitempty"`
	StderrEncoding  string `json:"stderr_encoding,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
}

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// Metrics returns the engine's Prometheus metrics, as text.
func (c *Client) Metrics(ctx context.Context) (string, error) {
	var buf bytes.Buffer
	err := c.doRaw(ctx, http.MethodGet, "/metrics", nil, &buf)
	return buf.String(), err
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var out Info
	return &out, c.do(ctx, http.MethodGet, "/v1/info", nil, &out)
}

func (c *Client) CreateWorkspace(ctx context.Context, req CreateWorkspace) (*Workspace, error) {
	var out Workspace
	return &out, c.do(ctx, http.MethodPost, "/v1/workspaces", req, &out)
}

func (c *Client) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	var out struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	return out.Workspaces, c.do(ctx, http.MethodGet, "/v1/workspaces", nil, &out)
}

func (c *Client) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	var out Workspace
	return &out, c.do(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(id), nil, &out)
}

// PauseWorkspace frees a workspace's memory now (a full save the engine keeps
// for itself); the next command or write wakes it with its processes. Pausing a
// paused or stopped workspace does nothing.
func (c *Client) PauseWorkspace(ctx context.Context, id string) (*Workspace, error) {
	var out Workspace
	return &out, c.do(ctx, http.MethodPost, "/v1/workspaces/"+url.PathEscape(id)+"/pause", nil, &out)
}

func (c *Client) DeleteWorkspace(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/workspaces/"+url.PathEscape(id), nil, nil)
}

func (c *Client) Exec(ctx context.Context, id string, req Exec) (*ExecResult, error) {
	var out ExecResult
	if err := c.do(ctx, http.MethodPost, "/v1/workspaces/"+url.PathEscape(id)+"/exec", req, &out); err != nil {
		return nil, err
	}
	// binary output arrives base64-encoded; hand back the exact bytes
	for _, f := range []struct {
		enc *string
		val *string
	}{{&out.StdoutEncoding, &out.Stdout}, {&out.StderrEncoding, &out.Stderr}} {
		if *f.enc == "base64" {
			b, err := base64.StdEncoding.DecodeString(*f.val)
			if err != nil {
				return nil, fmt.Errorf("decode output: %w", err)
			}
			*f.val, *f.enc = string(b), ""
		}
	}
	return &out, nil
}

// Session is an interpreter kept running in a workspace: variables, imports,
// loaded data and the working folder carry over between runs, and full saves
// keep it like any other process.
type Session struct {
	ID       string `json:"id"`
	Language string `json:"language"` // "python" or "shell"
	PID      int    `json:"pid"`
	Cells    int    `json:"cells"`
}

// RunResult is how a cell ended. In Python sessions the value of a final
// expression is printed, as in a notebook, and an exception's traceback goes
// to Stderr with ExitCode 1. TimedOut: the cell was interrupted at its
// timeout and the session carries on. Ended: the session's process died
// during the cell; start a new session.
type RunResult struct {
	ExecResult
	Ended bool `json:"ended,omitempty"`
}

// CreateSession starts a "python" or "shell" session in a workspace.
func (c *Client) CreateSession(ctx context.Context, id, language string) (*Session, error) {
	var out Session
	return &out, c.do(ctx, http.MethodPost, "/v1/workspaces/"+url.PathEscape(id)+"/sessions", map[string]string{"language": language}, &out)
}

// ListSessions lists a workspace's live sessions.
func (c *Client) ListSessions(ctx context.Context, id string) ([]Session, error) {
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	return out.Sessions, c.do(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(id)+"/sessions", nil, &out)
}

// RunInSession runs code in a session; timeout 0 means the engine's default.
func (c *Client) RunInSession(ctx context.Context, id, session, code string, timeout time.Duration) (*RunResult, error) {
	var out RunResult
	body := map[string]any{"code": code}
	if timeout > 0 {
		body["timeout_ms"] = timeout.Milliseconds()
	}
	if err := c.do(ctx, http.MethodPost, "/v1/workspaces/"+url.PathEscape(id)+"/sessions/"+url.PathEscape(session)+"/run", body, &out); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		enc *string
		val *string
	}{{&out.StdoutEncoding, &out.Stdout}, {&out.StderrEncoding, &out.Stderr}} {
		if *f.enc == "base64" {
			b, err := base64.StdEncoding.DecodeString(*f.val)
			if err != nil {
				return nil, fmt.Errorf("decode output: %w", err)
			}
			*f.val, *f.enc = string(b), ""
		}
	}
	return &out, nil
}

// DeleteSession stops a session and everything it started.
func (c *Client) DeleteSession(ctx context.Context, id, session string) error {
	return c.do(ctx, http.MethodDelete, "/v1/workspaces/"+url.PathEscape(id)+"/sessions/"+url.PathEscape(session), nil, nil)
}

// ExecEvent is one piece of a streamed command: output from Stream ("stdout" or
// "stderr") as it was written.
type ExecEvent struct {
	Stream string
	Data   []byte
}

// ExecStream runs a command and calls fn with its output as it is produced,
// then returns how it ended. ExecResult's Stdout and Stderr are empty: the
// output went to fn. fn is called from one goroutine, in order.
func (c *Client) ExecStream(ctx context.Context, id string, req Exec, fn func(ExecEvent)) (*ExecResult, error) {
	body := struct {
		Exec
		Stream bool `json:"stream"`
	}{req, true}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/workspaces/"+url.PathEscape(id)+"/exec", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, readError(resp)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var ev struct {
			Type       string            `json:"type"`
			Data       []byte            `json:"data"`
			ExitCode   *int              `json:"exit_code"`
			TimedOut   bool              `json:"timed_out"`
			DurationMS int64             `json:"duration_ms"`
			Error      map[string]string `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("bad stream event: %w", err)
		}
		switch ev.Type {
		case "stdout", "stderr":
			if fn != nil {
				fn(ExecEvent{Stream: ev.Type, Data: ev.Data})
			}
		case "exit":
			res := &ExecResult{TimedOut: ev.TimedOut, DurationMS: ev.DurationMS}
			if ev.ExitCode != nil {
				res.ExitCode = *ev.ExitCode
			}
			return res, nil
		case "error":
			return nil, &Error{Status: resp.StatusCode, Code: ev.Error["code"], Message: ev.Error["message"]}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("stream ended without an exit event")
}

// Run is Exec for the common case: run argv and return stdout, failing on a
// non-zero exit.
func (c *Client) Run(ctx context.Context, id string, argv ...string) (string, error) {
	res, err := c.Exec(ctx, id, Exec{Command: argv})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 || res.TimedOut {
		return res.Stdout, fmt.Errorf("%q exited %d (timed out: %v): %s", strings.Join(argv, " "), res.ExitCode, res.TimedOut, res.Stderr)
	}
	return res.Stdout, nil
}

func (c *Client) WriteFile(ctx context.Context, id, path string, content []byte) error {
	q := url.Values{"path": {path}}
	return c.doRaw(ctx, http.MethodPut, "/v1/workspaces/"+url.PathEscape(id)+"/files?"+q.Encode(), bytes.NewReader(content), nil)
}

// WriteFileFrom streams r into a file in the workspace, for files too big to
// hold in memory.
func (c *Client) WriteFileFrom(ctx context.Context, id, path string, r io.Reader) error {
	q := url.Values{"path": {path}}
	return c.doRaw(ctx, http.MethodPut, "/v1/workspaces/"+url.PathEscape(id)+"/files?"+q.Encode(), r, nil)
}

// ReadFileTo streams a file out of the workspace into w.
func (c *Client) ReadFileTo(ctx context.Context, id, path string, w io.Writer) error {
	q := url.Values{"path": {path}}
	return c.doRaw(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(id)+"/files?"+q.Encode(), nil, w)
}

func (c *Client) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	q := url.Values{"path": {path}}
	var buf bytes.Buffer
	err := c.doRaw(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(id)+"/files?"+q.Encode(), nil, &buf)
	return buf.Bytes(), err
}

func (c *Client) Save(ctx context.Context, id string, kind SaveKind) (*Save, error) {
	var out Save
	return &out, c.do(ctx, http.MethodPost, "/v1/workspaces/"+url.PathEscape(id)+"/saves", map[string]SaveKind{"kind": kind}, &out)
}

func (c *Client) ListSaves(ctx context.Context, workspaceID string) ([]Save, error) {
	var out struct {
		Saves []Save `json:"saves"`
	}
	return out.Saves, c.do(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/saves", nil, &out)
}

func (c *Client) GetSave(ctx context.Context, id string) (*Save, error) {
	var out Save
	return &out, c.do(ctx, http.MethodGet, "/v1/saves/"+url.PathEscape(id), nil, &out)
}

// ImportTemplate uploads a root filesystem tar (for example
// `docker export $(docker create python:3.12-slim)`) as template name.
func (c *Client) ImportTemplate(ctx context.Context, name string, rootfsTar io.Reader) (*Template, error) {
	var buf bytes.Buffer
	if err := c.doRaw(ctx, http.MethodPut, "/v1/templates/"+url.PathEscape(name), rootfsTar, &buf); err != nil {
		return nil, err
	}
	var t Template
	return &t, json.Unmarshal(buf.Bytes(), &t)
}

// PullTemplate builds template name from a container image in a registry (the
// image for the engine's architecture, all layers applied).
func (c *Client) PullTemplate(ctx context.Context, name string, req PullTemplate) (*Template, error) {
	var out Template
	return &out, c.do(ctx, http.MethodPost, "/v1/templates/"+url.PathEscape(name)+"/pull", req, &out)
}

func (c *Client) ListTemplates(ctx context.Context) ([]Template, error) {
	var out struct {
		Templates []Template `json:"templates"`
	}
	return out.Templates, c.do(ctx, http.MethodGet, "/v1/templates", nil, &out)
}

func (c *Client) DeleteTemplate(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/templates/"+url.PathEscape(name), nil, nil)
}

// DeleteSave hides a save. It can be undeleted until its PurgeAfter time.
func (c *Client) DeleteSave(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/saves/"+url.PathEscape(id), nil, nil)
}

// UndeleteSave brings back a deleted save within its retention window.
func (c *Client) UndeleteSave(ctx context.Context, id string) (*Save, error) {
	var out Save
	return &out, c.do(ctx, http.MethodPost, "/v1/saves/"+url.PathEscape(id)+"/undelete", nil, &out)
}

// VerifySave re-checks a save's checksums on the engine and returns its state:
// "ready", or "corrupt" / "missing" if its files are damaged or gone.
func (c *Client) VerifySave(ctx context.Context, id string) (*Save, error) {
	var out Save
	return &out, c.do(ctx, http.MethodPost, "/v1/saves/"+url.PathEscape(id)+"/verify", nil, &out)
}

func (c *Client) Resume(ctx context.Context, saveID string) (*Workspace, error) {
	var out Workspace
	return &out, c.do(ctx, http.MethodPost, "/v1/saves/"+url.PathEscape(saveID)+"/resume", nil, &out)
}

func (c *Client) Copy(ctx context.Context, saveID string, count int) ([]Workspace, error) {
	var out struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	return out.Workspaces, c.do(ctx, http.MethodPost, "/v1/saves/"+url.PathEscape(saveID)+"/copies", map[string]int{"count": count}, &out)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	if out == nil {
		return c.doRaw(ctx, method, path, body, nil)
	}
	var buf bytes.Buffer
	if err := c.doRaw(ctx, method, path, body, &buf); err != nil {
		return err
	}
	return json.Unmarshal(buf.Bytes(), out)
}

func (c *Client) doRaw(ctx context.Context, method, path string, body io.Reader, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if key, ok := ctx.Value(idempotencyKeyCtx{}).(string); ok && key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return readError(resp)
	}
	if out != nil {
		_, err = io.Copy(out, resp.Body)
	}
	return err
}

func readError(resp *http.Response) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	id := resp.Header.Get("X-Request-Id")
	if json.Unmarshal(data, &e) != nil || e.Error.Code == "" {
		return &Error{Status: resp.StatusCode, Code: "http_error", Message: strings.TrimSpace(string(data)), RequestID: id}
	}
	return &Error{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message, RequestID: id}
}
