package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/AQLD/cairn/internal/engine"
)

// GET /v1/workspaces/{id}/events: the workspace's activity log, oldest first.
//
//	?after=ID    only events after this one (a cursor: pass the last ID seen)
//	?limit=N     at most N (default 100, at most 1000)
//	?last=N      the newest N instead of the oldest after the cursor
//	?kind=K      only this kind ("exec"; "session_*" for a prefix)
//	?follow=1    stream: newline-delimited JSON events, the existing ones
//	             (from after/last) then new ones as they happen, until the
//	             client disconnects or the engine shuts down
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var eq engine.EventQuery
	var err error
	if v := q.Get("after"); v != "" {
		if eq.After, err = strconv.ParseInt(v, 10, 64); err != nil || eq.After < 0 {
			writeError(w, http.StatusBadRequest, "invalid_argument", "after must be an event ID")
			return
		}
	}
	for _, p := range []struct {
		name string
		dst  *int
	}{{"limit", &eq.Limit}, {"last", &eq.Limit}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				writeError(w, http.StatusBadRequest, "invalid_argument", p.name+" must be a positive number")
				return
			}
			*p.dst = n
			eq.Tail = eq.Tail || p.name == "last"
		}
	}
	eq.Kind = q.Get("kind")
	id := r.PathValue("id")
	seen := s.eng.EventsLast() // before listing, so nothing slips between
	evs, err := s.eng.Events(r.Context(), id, eq)
	if err != nil {
		s.fail(w, err)
		return
	}
	if q.Get("follow") != "1" && q.Get("follow") != "true" {
		writeJSON(w, http.StatusOK, map[string]any{"events": evs})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	send := func(evs []engine.Event) bool {
		for _, ev := range evs {
			if enc.Encode(ev) != nil {
				return false
			}
			eq.After = ev.ID
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		return true
	}
	if !send(evs) {
		return
	}
	eq.Tail = false
	full := len(evs) == pageSize(eq.Limit) && q.Get("last") == ""
	for {
		if full { // more were waiting than one page: no need to wait
			evs, err := s.eng.Events(r.Context(), id, eq)
			if err != nil || !send(evs) {
				return
			}
			full = len(evs) == pageSize(eq.Limit)
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.stopping():
			return
		case <-s.eng.EventsChanged(seen):
		}
		seen = s.eng.EventsLast()
		evs, err := s.eng.Events(r.Context(), id, eq)
		if err != nil {
			return // the engine closed its log, or the client went away
		}
		if !send(evs) {
			return
		}
		full = len(evs) == pageSize(eq.Limit)
	}
}

// pageSize is how many events one list returns for a limit (see eventlog.Query).
func pageSize(limit int) int {
	if limit <= 0 {
		return 100
	}
	return min(limit, 1000)
}

// StopStreams ends long-lived responses (followed activity logs) so a server
// shutdown is not held up by them. Pass it to http.Server.RegisterOnShutdown.
func (s *Server) StopStreams() { s.stopO.Do(func() { close(s.stop) }) }

func (s *Server) stopping() <-chan struct{} { return s.stop }
