package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Event is one entry of a workspace's activity log: a command run, a file
// written or read, a save, resume, copy, pause or wake, a session cell.
type Event struct {
	ID          int64          `json:"id"` // increasing: a cursor for After
	WorkspaceID string         `json:"workspace_id"`
	Time        time.Time      `json:"time"`
	Kind        string         `json:"kind"` // exec, file_write, file_read, save, resume, copy, create, delete, pause, wake, session_start, session_run, session_stop
	RequestID   string         `json:"request_id,omitempty"`
	Key         string         `json:"key,omitempty"`   // who acted: an API key's ID, or "operator"
	Summary     string         `json:"summary"`         // one readable line
	Error       string         `json:"error,omitempty"` // the operation failed
	Data        map[string]any `json:"data,omitempty"`  // kind-specific details (exit_code, stdout_tail, save, …)
}

// EventQuery selects events. The zero value is the oldest 100.
type EventQuery struct {
	After int64  // only events after this ID
	Limit int    // at most this many (default 100, at most 1000)
	Last  int    // the newest Last events instead (overrides Limit)
	Kind  string // only this kind; "session_*" for a prefix
}

func (q EventQuery) values() url.Values {
	v := url.Values{}
	if q.After > 0 {
		v.Set("after", strconv.FormatInt(q.After, 10))
	}
	if q.Last > 0 {
		v.Set("last", strconv.Itoa(q.Last))
	} else if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Kind != "" {
		v.Set("kind", q.Kind)
	}
	return v
}

// Events lists a workspace's activity, oldest first. Page with After set to
// the last ID returned.
func (c *Client) Events(ctx context.Context, workspaceID string, q EventQuery) ([]Event, error) {
	var out struct {
		Events []Event `json:"events"`
	}
	path := "/v1/workspaces/" + url.PathEscape(workspaceID) + "/events"
	if v := q.values().Encode(); v != "" {
		path += "?" + v
	}
	return out.Events, c.do(ctx, http.MethodGet, path, nil, &out)
}

// FollowEvents calls fn with the events q selects, then with each new one as
// it happens, until ctx ends (it then returns nil), fn returns an error, or
// the engine ends the stream (it shuts down: an error).
func (c *Client) FollowEvents(ctx context.Context, workspaceID string, q EventQuery, fn func(Event) error) error {
	return c.follow(ctx, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/events", q, fn)
}

// AllEvents lists events across every workspace the caller may see and the
// engine's own (disk pressure), oldest first.
func (c *Client) AllEvents(ctx context.Context, q EventQuery) ([]Event, error) {
	var out struct {
		Events []Event `json:"events"`
	}
	return out.Events, c.do(ctx, http.MethodGet, "/v1/events?"+q.values().Encode(), nil, &out)
}

// FollowAllEvents calls fn for every event across the workspaces the caller
// may see, as they happen: pauses, wakes, limits hit, commands finished.
// It returns when ctx ends, fn returns an error, or the engine ends the stream.
func (c *Client) FollowAllEvents(ctx context.Context, q EventQuery, fn func(Event) error) error {
	return c.follow(ctx, "/v1/events", q, fn)
}

func (c *Client) follow(ctx context.Context, path string, q EventQuery, fn func(Event) error) error {
	v := q.values()
	v.Set("follow", "1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+v.Encode(), nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return readError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return fmt.Errorf("bad event: %w", err)
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("the engine ended the event stream")
}

// Audit lists what was done across every workspace and the engine itself
// (keys, secrets), oldest first; key, if set, keeps one caller's actions.
// Needs an admin token.
func (c *Client) Audit(ctx context.Context, key string, q EventQuery) ([]Event, error) {
	v := q.values()
	if key != "" {
		v.Set("key", key)
	}
	var out struct {
		Events []Event `json:"events"`
	}
	return out.Events, c.do(ctx, http.MethodGet, "/v1/audit?"+v.Encode(), nil, &out)
}
