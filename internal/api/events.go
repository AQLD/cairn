package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/AQLD/cairn/internal/engine"
)

// audit lists what was done across workspaces and the engine (admin only),
// optionally by one key: GET /v1/audit?key=key_…&kind=&after=&limit=&last=.
func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	eq, ok := eventQuery(w, r)
	if !ok {
		return
	}
	eq.Key = r.URL.Query().Get("key")
	evs, err := s.eng.Audit(r.Context(), eq)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}

// eventQuery reads after, limit, last and kind.
func eventQuery(w http.ResponseWriter, r *http.Request) (engine.EventQuery, bool) {
	q := r.URL.Query()
	var eq engine.EventQuery
	var err error
	if v := q.Get("after"); v != "" {
		if eq.After, err = strconv.ParseInt(v, 10, 64); err != nil || eq.After < 0 {
			writeError(w, http.StatusBadRequest, "invalid_argument", "after must be an event ID")
			return eq, false
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
				return eq, false
			}
			*p.dst = n
			eq.Tail = eq.Tail || p.name == "last"
		}
	}
	eq.Kind = q.Get("kind")
	return eq, true
}

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
	id := r.PathValue("id")
	s.events(w, r, func(eq engine.EventQuery) ([]engine.Event, int64, error) {
		evs, err := s.eng.Events(r.Context(), id, eq)
		cursor := eq.After
		if len(evs) > 0 {
			cursor = evs[len(evs)-1].ID
		}
		return evs, cursor, err
	})
}

// GET /v1/events: the same across every workspace the caller may see, and
// the engine's own events (disk pressure; key and secret changes for admins),
// with the same parameters. With ?follow=1 a client hears when a workspace
// pauses, wakes, hits a limit or finishes a command.
func (s *Server) allEvents(w http.ResponseWriter, r *http.Request) {
	s.events(w, r, func(eq engine.EventQuery) ([]engine.Event, int64, error) {
		return s.eng.AllEvents(r.Context(), eq)
	})
}

// events answers a list or, with ?follow=1, a stream. list returns events
// and the cursor to continue from (past any the caller may not see).
func (s *Server) events(w http.ResponseWriter, r *http.Request, list func(engine.EventQuery) ([]engine.Event, int64, error)) {
	eq, ok := eventQuery(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	seen := s.eng.EventsLast() // before listing, so nothing slips between
	evs, cursor, err := list(eq)
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
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		return true
	}
	if !send(evs) {
		return
	}
	// a tail of the newest events continues after the newest one
	if eq.Tail && len(evs) > 0 {
		cursor = max(cursor, evs[len(evs)-1].ID)
	}
	eq.After, eq.Tail = cursor, false
	full := cursor > 0 && len(evs) == pageSize(eq.Limit) && q.Get("last") == ""
	for {
		if !full {
			select {
			case <-r.Context().Done():
				return
			case <-s.stopping():
				return
			case <-s.eng.EventsChanged(seen):
			}
			seen = s.eng.EventsLast()
		}
		evs, cursor, err := list(eq)
		if err != nil || !send(evs) {
			return // the engine closed its log, or the client went away
		}
		full = cursor > eq.After && cursor < seen // more were waiting than one page
		eq.After = cursor
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
