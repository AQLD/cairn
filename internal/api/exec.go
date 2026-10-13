package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AQLD/cairn/internal/runtime"
)

const (
	maxOutputBytes = 4 << 20 // per stream, per command, when not streaming
	defaultTimeout = 10 * time.Minute
)

// ExecRequest is the body of POST /v1/workspaces/{id}/exec.
type ExecRequest struct {
	Command   []string          `json:"command"`
	Env       map[string]string `json:"env,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Stdin     string            `json:"stdin,omitempty"`
	TimeoutMS int64             `json:"timeout_ms,omitempty"`
	// Stream returns output as it is produced, as newline-delimited JSON
	// (application/x-ndjson): one ExecEvent per line, ending with an "exit" event.
	Stream bool `json:"stream,omitempty"`
}

// ExecResponse is the result of a command that was not streamed. Output beyond
// 4 MiB per stream is cut off and marked truncated. Output that is not valid
// UTF-8 is sent base64-encoded, with its *_encoding set to "base64".
type ExecResponse struct {
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

// ExecEvent is one line of a streamed command.
//
//	{"type":"stdout","data":"<base64>"}
//	{"type":"stderr","data":"<base64>"}
//	{"type":"exit","exit_code":0,"timed_out":false,"duration_ms":12}
//	{"type":"error","error":{"code":"internal","message":"..."}}   (the command could not finish)
type ExecEvent struct {
	Type       string            `json:"type"`
	Data       []byte            `json:"data,omitempty"` // base64 in JSON: output is bytes, not text
	ExitCode   *int              `json:"exit_code,omitempty"`
	TimedOut   bool              `json:"timed_out,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
	Error      map[string]string `json:"error,omitempty"`
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if !s.decode(w, r, &req) {
		return
	}
	timeout := defaultTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if req.Stream {
		s.execStream(w, r, req, timeout)
		return
	}

	stdout, stderr := &capped{max: maxOutputBytes}, &capped{max: maxOutputBytes}
	start := time.Now()
	res, err := s.eng.Exec(r.Context(), r.PathValue("id"), runtime.ExecRequest{
		Argv: req.Command, Env: req.Env, Dir: req.Dir, Stdin: strings.NewReader(req.Stdin),
		Stdout: stdout, Stderr: stderr, Timeout: timeout,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := ExecResponse{
		ExitCode: res.ExitCode, TimedOut: res.TimedOut,
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}
	resp.Stdout, resp.StdoutEncoding = encodeOutput(stdout.buf.Bytes())
	resp.Stderr, resp.StderrEncoding = encodeOutput(stderr.buf.Bytes())
	writeJSON(w, http.StatusOK, resp)
}

// encodeOutput keeps text readable and binary exact.
func encodeOutput(b []byte) (string, string) {
	if utf8.Valid(b) {
		return string(b), ""
	}
	return base64.StdEncoding.EncodeToString(b), "base64"
}

func (s *Server) execStream(w http.ResponseWriter, r *http.Request, req ExecRequest, timeout time.Duration) {
	st := &stream{w: w, rc: http.NewResponseController(w)}
	start := time.Now()
	res, err := s.eng.Exec(r.Context(), r.PathValue("id"), runtime.ExecRequest{
		Argv: req.Command, Env: req.Env, Dir: req.Dir, Stdin: strings.NewReader(req.Stdin),
		Stdout: st.writer("stdout"), Stderr: st.writer("stderr"), Timeout: timeout,
	})
	if err != nil {
		if !st.started() {
			s.fail(w, err) // nothing sent yet: a normal error response with its status
			return
		}
		s.log.Error("streamed command failed", "err", err)
		st.send(ExecEvent{Type: "error", Error: map[string]string{"code": "internal", "message": "command failed: " + err.Error()}})
		return
	}
	code := res.ExitCode
	st.send(ExecEvent{Type: "exit", ExitCode: &code, TimedOut: res.TimedOut, DurationMS: time.Since(start).Milliseconds()})
}

// stream writes ExecEvents as NDJSON, flushing each one. Headers go out with
// the first event, so errors before any output still get a proper status.
type stream struct {
	mu    sync.Mutex
	w     http.ResponseWriter
	rc    *http.ResponseController
	begun bool
	err   error
}

func (st *stream) started() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.begun
}

func (st *stream) send(ev ExecEvent) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err != nil {
		return st.err
	}
	if !st.begun {
		st.w.Header().Set("Content-Type", "application/x-ndjson")
		st.w.Header().Set("X-Content-Type-Options", "nosniff")
		st.w.WriteHeader(http.StatusOK)
		st.begun = true
	}
	if err := json.NewEncoder(st.w).Encode(ev); err != nil {
		st.err = err
		return err
	}
	if err := st.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		st.err = err
	}
	return st.err
}

func (st *stream) writer(kind string) *streamWriter { return &streamWriter{st: st, kind: kind} }

type streamWriter struct {
	st   *stream
	kind string
}

// Write sends one event per chunk the command writes. If the client has gone
// away it keeps accepting output so the command is never blocked on a dead
// connection; the request context cancels the command instead.
func (sw *streamWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		_ = sw.st.send(ExecEvent{Type: sw.kind, Data: bytes.Clone(p)})
	}
	return len(p), nil
}

// capped keeps the first max bytes written and notes whether more arrived.
type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil // keep draining so the command never blocks on a full pipe
}
