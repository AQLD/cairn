package api

import (
	"net/http"
	"time"

	"github.com/AQLD/cairn/internal/engine"
)

// SessionRequest is the body of POST /v1/workspaces/{id}/sessions.
type SessionRequest struct {
	Language string `json:"language"` // "python" or "shell"
}

// RunRequest is the body of POST /v1/workspaces/{id}/sessions/{sid}/run.
type RunRequest struct {
	Code      string `json:"code"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

// RunResponse is how a cell ended, with its output encoded as for exec.
// Ended means the session's process died during the cell.
type RunResponse struct {
	ExecResponse
	Ended bool `json:"ended,omitempty"`
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req SessionRequest
	if !s.decode(w, r, &req) {
		return
	}
	ses, err := s.eng.CreateSession(r.Context(), r.PathValue("id"), req.Language)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ses)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.eng.ListSessions(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

func (s *Server) runInSession(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if !s.decode(w, r, &req) {
		return
	}
	timeout := defaultTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	stdout, stderr := &capped{max: maxOutputBytes}, &capped{max: maxOutputBytes}
	start := time.Now()
	res, err := s.eng.RunInSession(r.Context(), r.PathValue("id"), r.PathValue("sid"), engine.SessionRun{
		Code: req.Code, Stdout: stdout, Stderr: stderr, Timeout: timeout,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := RunResponse{Ended: res.Ended, ExecResponse: ExecResponse{
		ExitCode: res.ExitCode, TimedOut: res.TimedOut,
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}}
	resp.Stdout, resp.StdoutEncoding = encodeOutput(stdout.buf.Bytes())
	resp.Stderr, resp.StderrEncoding = encodeOutput(stderr.buf.Bytes())
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.DeleteSession(r.Context(), r.PathValue("id"), r.PathValue("sid")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
