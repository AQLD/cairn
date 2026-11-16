package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/api"
	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/runtime"
)

// conn runs a server over pipes against a real engine (host runtime).
type conn struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
}

func start(t *testing.T) *conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng, err := engine.Open(ctx, engine.Config{DataDir: t.TempDir(), Runtime: runtime.NewHost()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	srv := httptest.NewServer(api.New(eng, slog.New(slog.NewTextHandler(io.Discard, nil)), ""))
	t.Cleanup(srv.Close)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := New(client.New(srv.URL, ""), "test", "", nil)
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, inR, outW); outW.Close() }()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop when stdin closed")
		}
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	return &conn{t: t, in: inW, out: sc}
}

func (c *conn) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *conn) recv() map[string]any {
	c.t.Helper()
	if !c.out.Scan() {
		c.t.Fatal("no response")
	}
	var m map[string]any
	if err := json.Unmarshal(c.out.Bytes(), &m); err != nil {
		c.t.Fatalf("not JSON: %q", c.out.Text())
	}
	return m
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	c := start(t)
	for _, tc := range []struct{ asked, want string }{
		{"2024-11-05", "2024-11-05"},
		{"2025-06-18", "2025-06-18"},
		{"1999-01-01", protocolVersions[0]},
		{"", protocolVersions[0]},
	} {
		c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + tc.asked + `"}}`)
		r := c.recv()["result"].(map[string]any)
		if r["protocolVersion"] != tc.want {
			t.Errorf("asked %q: got %v, want %s", tc.asked, r["protocolVersion"], tc.want)
		}
		if !strings.Contains(r["instructions"].(string), "rewind") {
			t.Error("instructions should tell the agent when to rewind")
		}
	}
}

func TestNotificationsGetNoReplyAndErrorsHaveCodes(t *testing.T) {
	c := start(t)
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":99}}`)
	c.send(`{"jsonrpc":"2.0","id":"a","method":"ping"}`)
	if m := c.recv(); m["id"] != "a" { // the notifications produced nothing before it
		t.Fatalf("got %v, want the ping's reply", m)
	}
	c.send(`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`)
	if e, _ := c.recv()["error"].(map[string]any); e["code"] != float64(errMethodNotFound) {
		t.Fatalf("unknown method: %v", e)
	}
	c.send(`not json`)
	if e, _ := c.recv()["error"].(map[string]any); e["code"] != float64(errParse) {
		t.Fatalf("parse error: %v", e)
	}
	c.send(`[{"jsonrpc":"2.0","id":3,"method":"ping"},{"jsonrpc":"2.0","id":4,"method":"ping"}]`)
	ids := map[float64]bool{}
	ids[c.recv()["id"].(float64)] = true
	ids[c.recv()["id"].(float64)] = true
	if !ids[3] || !ids[4] {
		t.Fatalf("batch replies %v", ids)
	}
}

func TestToolErrorsAreResultsNotProtocolErrors(t *testing.T) {
	c := start(t)
	// no workspace yet: the tool says what to do
	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":{"command":"true"}}}`)
	r := c.recv()["result"].(map[string]any)
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if r["isError"] != true || !strings.Contains(text, "workspace_create") {
		t.Fatalf("got %v", r)
	}
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file","arguments":{"workspace":"ws_missing","path":"x"}}}`)
	r = c.recv()["result"].(map[string]any)
	if text := r["content"].([]any)[0].(map[string]any)["text"].(string); r["isError"] != true || !strings.Contains(text, "not_found") {
		t.Fatalf("engine error: %v", r)
	}
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if c.recv()["error"] == nil {
		t.Fatal("unknown tool should be a protocol error")
	}
}

func TestToolList(t *testing.T) {
	if len(tools) != len(order) {
		t.Fatalf("%d tools registered, %d ordered", len(tools), len(order))
	}
	for i, tl := range toolList() {
		if tl.Name != order[i] {
			t.Fatalf("tool %d is %s, want %s", i, tl.Name, order[i])
		}
	}
}

func TestClipKeepsStartAndEnd(t *testing.T) {
	s := "START" + strings.Repeat("x", 50000) + "END"
	got := clip(s, 1000, "advice")
	if len(got) > 1100 || !strings.HasPrefix(got, "START") || !strings.HasSuffix(got, "END") || !strings.Contains(got, "advice") {
		t.Fatalf("clip: %d chars %q...%q", len(got), got[:20], got[len(got)-20:])
	}
	if clip("short", 1000, "") != "short" {
		t.Fatal("short text changed")
	}
}
