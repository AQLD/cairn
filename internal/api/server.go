// Package api is Cairn's REST API. The CLI, SDKs, MCP server and tests all
// talk to the engine through it.
//
//	GET    /healthz
//	GET    /v1/info
//	POST   /v1/workspaces                   create
//	GET    /v1/workspaces                   list
//	GET    /v1/workspaces/{id}              get
//	DELETE /v1/workspaces/{id}              delete (saves are kept)
//	POST   /v1/workspaces/{id}/exec         run a command ({"stream": true} for live NDJSON output)
//	PUT    /v1/workspaces/{id}/files?path=  write a file (body is the content)
//	GET    /v1/workspaces/{id}/files?path=  read a file
//	POST   /v1/workspaces/{id}/saves        save {"kind": "quick" | "full"}
//	GET    /v1/workspaces/{id}/saves        list saves
//	GET    /v1/saves/{id}                   get a save
//	POST   /v1/saves/{id}/resume            restore the workspace to this save
//	POST   /v1/saves/{id}/copies            copy {"count": n}
//	DELETE /v1/saves/{id}                   delete (recoverable until purge_after)
//	POST   /v1/saves/{id}/undelete          bring back a deleted save
//
// Errors are {"error": {"code": "...", "message": "..."}}.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/save"
)

const (
	maxJSONBody  = 1 << 20 // 1 MiB
	maxFileBytes = 1 << 30 // 1 GiB per upload
)

type Server struct {
	eng   *engine.Engine
	log   *slog.Logger
	token string
	mux   *http.ServeMux
}

// New returns the API handler. If token is set, every request except /healthz
// must carry "Authorization: Bearer <token>".
func New(eng *engine.Engine, log *slog.Logger, token string) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{eng: eng, log: log, token: token, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && r.URL.Path != "/healthz" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or wrong API token")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.eng.Info())
	})
	s.mux.HandleFunc("POST /v1/workspaces", s.createWorkspace)
	s.mux.HandleFunc("GET /v1/workspaces", s.listWorkspaces)
	s.mux.HandleFunc("GET /v1/workspaces/{id}", s.getWorkspace)
	s.mux.HandleFunc("DELETE /v1/workspaces/{id}", s.deleteWorkspace)
	s.mux.HandleFunc("POST /v1/workspaces/{id}/exec", s.exec)
	s.mux.HandleFunc("PUT /v1/workspaces/{id}/files", s.writeFile)
	s.mux.HandleFunc("GET /v1/workspaces/{id}/files", s.readFile)
	s.mux.HandleFunc("POST /v1/workspaces/{id}/saves", s.createSave)
	s.mux.HandleFunc("GET /v1/workspaces/{id}/saves", s.listSaves)
	s.mux.HandleFunc("GET /v1/saves/{id}", s.getSave)
	s.mux.HandleFunc("POST /v1/saves/{id}/resume", s.resume)
	s.mux.HandleFunc("POST /v1/saves/{id}/copies", s.copySave)
	s.mux.HandleFunc("DELETE /v1/saves/{id}", s.deleteSave)
	s.mux.HandleFunc("POST /v1/saves/{id}/undelete", s.undeleteSave)
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req engine.CreateRequest
	if !s.decode(w, r, &req) {
		return
	}
	ws, err := s.eng.CreateWorkspace(r.Context(), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	ws, err := s.eng.ListWorkspaces(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": ws})
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.eng.GetWorkspace(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.DeleteWorkspace(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	mode := os.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument", "mode must be octal, like 755")
			return
		}
		mode = os.FileMode(v)
	}
	body := http.MaxBytesReader(w, r.Body, maxFileBytes)
	if err := s.eng.WriteFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), body, mode); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	f, info, err := s.eng.OpenFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	_, _ = io.Copy(w, f)
}

func (s *Server) createSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind save.Kind `json:"kind"`
	}
	if r.ContentLength != 0 && !s.decode(w, r, &req) {
		return
	}
	sv, err := s.eng.Save(r.Context(), r.PathValue("id"), req.Kind)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sv)
}

func (s *Server) listSaves(w http.ResponseWriter, r *http.Request) {
	svs, err := s.eng.ListSaves(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saves": svs})
}

func (s *Server) getSave(w http.ResponseWriter, r *http.Request) {
	sv, err := s.eng.GetSave(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sv)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	ws, err := s.eng.Resume(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) copySave(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Count int `json:"count"`
	}{Count: 1}
	if r.ContentLength != 0 && !s.decode(w, r, &req) {
		return
	}
	ws, err := s.eng.Copy(r.Context(), r.PathValue("id"), req.Count)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workspaces": ws})
}

func (s *Server) deleteSave(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.DeleteSave(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) undeleteSave(w http.ResponseWriter, r *http.Request) {
	sv, err := s.eng.UndeleteSave(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sv)
}

// --- plumbing ---

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_argument", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// fail maps engine errors to HTTP statuses and stable error codes.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, engine.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
	case errors.Is(err, engine.ErrCorrupt):
		writeError(w, http.StatusConflict, "save_corrupt", err.Error())
	case errors.Is(err, engine.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, engine.ErrUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported_by_runtime", err.Error())
	default:
		s.log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
