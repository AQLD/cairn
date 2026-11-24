package acceptance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// mcpConn is a coding agent's side of `cairn mcp`: the real CLI binary,
// spawned the way agents launch it and spoken to over its stdin and stdout.
type mcpConn struct {
	e      *Env
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	stderr *SyncBuffer
	next   int
}

var (
	cliOnce sync.Once
	cliPath string
	cliErr  error
)

// cairnCLI builds the cairn binary once per test run.
func cairnCLI(e *Env) string {
	e.T.Helper()
	cliOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cairn-cli-")
		if err != nil {
			cliErr = err
			return
		}
		cliPath = filepath.Join(dir, "cairn")
		out, err := exec.Command("go", "build", "-o", cliPath, "../cmd/cairn").CombinedOutput()
		if err != nil {
			cliErr = fmt.Errorf("build cairn: %v\n%s", err, out)
		}
	})
	if cliErr != nil {
		e.T.Fatal(cliErr)
	}
	return cliPath
}

func startMCP(e *Env, args ...string) *mcpConn {
	e.T.Helper()
	url, token := e.endpoint()
	cmd := exec.CommandContext(e.Ctx, cairnCLI(e), append([]string{"mcp"}, args...)...)
	cmd.Env = append(os.Environ(), "CAIRN_ENDPOINT="+url, "CAIRN_TOKEN="+token, "CAIRN_WORKSPACE=")
	in, err := cmd.StdinPipe()
	e.Must(err)
	out, err := cmd.StdoutPipe()
	e.Must(err)
	m := &mcpConn{e: e, cmd: cmd, in: in, out: bufio.NewScanner(out), stderr: &SyncBuffer{}}
	m.out.Buffer(make([]byte, 1<<20), 64<<20)
	cmd.Stderr = m.stderr
	e.Must(cmd.Start())
	e.T.Cleanup(func() { _ = in.Close(); _ = cmd.Wait() })
	return m
}

// send writes one message; recv reads one, failing on anything on stdout
// that is not a JSON-RPC message (it would break every MCP client).
func (m *mcpConn) send(msg map[string]any) {
	m.e.T.Helper()
	msg["jsonrpc"] = "2.0"
	data, err := json.Marshal(msg)
	m.e.Must(err)
	_, err = m.in.Write(append(data, '\n'))
	m.e.Must(err)
}

func (m *mcpConn) recv() map[string]any {
	m.e.T.Helper()
	if !m.out.Scan() {
		m.e.T.Fatalf("mcp server closed stdout (%v); stderr:\n%s", m.out.Err(), m.stderr.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(m.out.Bytes(), &msg); err != nil || msg["jsonrpc"] != "2.0" {
		m.e.T.Fatalf("stdout carried something that is not a JSON-RPC message: %q", m.out.Text())
	}
	return msg
}

// call sends a request and returns its response.
func (m *mcpConn) call(method string, params any) map[string]any {
	m.e.T.Helper()
	m.next++
	id := m.next
	m.send(map[string]any{"id": id, "method": method, "params": params})
	msg := m.recv()
	if got, _ := msg["id"].(float64); int(got) != id {
		m.e.T.Fatalf("response to request %d carries id %v", id, msg["id"])
	}
	return msg
}

// tool calls a tool and returns its text and whether it is a tool error.
func (m *mcpConn) tool(name string, args map[string]any) (string, bool) {
	m.e.T.Helper()
	msg := m.call("tools/call", map[string]any{"name": name, "arguments": args})
	if msg["error"] != nil {
		m.e.T.Fatalf("%s: protocol error %v", name, msg["error"])
	}
	res, _ := msg["result"].(map[string]any)
	parts, _ := res["content"].([]any)
	if len(parts) == 0 {
		m.e.T.Fatalf("%s: no content in %v", name, msg)
	}
	first, _ := parts[0].(map[string]any)
	if first["type"] != "text" {
		m.e.T.Fatalf("%s: first content is %v, want text", name, first["type"])
	}
	isErr, _ := res["isError"].(bool)
	return first["text"].(string), isErr
}

// ok calls a tool that must succeed.
func (m *mcpConn) ok(name string, args map[string]any) string {
	m.e.T.Helper()
	text, isErr := m.tool(name, args)
	if isErr {
		m.e.T.Fatalf("%s(%v) failed: %s", name, args, text)
	}
	return text
}

func (m *mcpConn) initialize() {
	m.e.T.Helper()
	res := m.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "acceptance", "version": "1"}})
	r, _ := res["result"].(map[string]any)
	Equal(m.e, "negotiated protocol version", r["protocolVersion"], any("2025-06-18"))
	if caps, _ := r["capabilities"].(map[string]any); caps["tools"] == nil {
		m.e.T.Fatalf("server does not offer tools: %v", r)
	}
	m.send(map[string]any{"method": "notifications/initialized"})
}

var (
	wsIDPattern   = regexp.MustCompile(`ws_[0-9a-f]+`)
	saveIDPattern = regexp.MustCompile(`sv_[0-9a-f]+`)
	toolName      = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
)

func firstMatch(e *Env, re *regexp.Regexp, text string) string {
	e.T.Helper()
	id := re.FindString(text)
	if id == "" {
		e.T.Fatalf("no %s in %q", re, text)
	}
	return id
}

func init() {
	// G10.12.1: an agent's core loop through MCP tools.
	register("G10.12.1", func(e *Env) {
		m := startMCP(e)
		m.initialize()
		Equal(e, "ping", fmt.Sprint(m.call("ping", map[string]any{})["result"]), "map[]")

		list := m.call("tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any)
		names := map[string]bool{}
		for _, t := range list {
			tm := t.(map[string]any)
			name := tm["name"].(string)
			if !toolName.MatchString(name) || len(tm["description"].(string)) < 40 {
				e.T.Errorf("tool %q: name or description unusable by agents", name)
			}
			if schema, _ := tm["inputSchema"].(map[string]any); schema["type"] != "object" {
				e.T.Errorf("tool %q: input schema is not an object", name)
			}
			names[name] = true
		}
		for _, want := range []string{"workspace_create", "exec", "write_file", "read_file", "push_dir", "pull_dir",
			"session_run", "save", "rewind", "copy", "list_saves", "diff", "history", "workspace_list", "pause", "workspace_delete"} {
			if !names[want] {
				e.T.Errorf("tool %s is missing", want)
			}
		}
		if len(list) > 16 {
			e.T.Errorf("%d tools: agents choose badly from long tool lists", len(list))
		}

		ws := firstMatch(e, wsIDPattern, m.ok("workspace_create", map[string]any{"name": "mcp-agent"}))
		m.ok("write_file", map[string]any{"path": "app/state.txt", "content": "one"})
		Equal(e, "read back", m.ok("read_file", map[string]any{"path": "app/state.txt"}), "one")
		out := m.ok("exec", map[string]any{"command": "cat app/state.txt; echo; echo to-stderr >&2"})
		if !strings.HasPrefix(out, "exit code 0") || !strings.Contains(out, "one") || !strings.Contains(out, "to-stderr") {
			e.T.Fatalf("exec result: %q", out)
		}
		// a failing command is a result, not a failed call
		if out := m.ok("exec", map[string]any{"command": "exit 7"}); !strings.HasPrefix(out, "exit code 7") {
			e.T.Fatalf("exit code: %q", out)
		}

		sv := firstMatch(e, saveIDPattern, m.ok("save", map[string]any{}))
		m.ok("write_file", map[string]any{"path": "app/state.txt", "content": "broken"})
		if out := m.ok("exec", map[string]any{"command": "false"}); !strings.Contains(out, sv) {
			e.T.Fatalf("a failing command should point at the last save to rewind to: %q", out)
		}
		saves := m.ok("list_saves", map[string]any{})
		if !strings.Contains(saves, sv) || !strings.Contains(saves, "current") {
			e.T.Fatalf("list_saves: %q", saves)
		}
		m.ok("rewind", map[string]any{"save": sv})
		Equal(e, "rewound", m.ok("read_file", map[string]any{"path": "app/state.txt"}), "one")

		copyText := m.ok("copy", map[string]any{"save": sv, "count": 2})
		if !strings.Contains(copyText, "workspace_delete") {
			e.T.Fatalf("copy does not tell the agent to delete the copies it does not keep: %s", copyText)
		}
		copies := wsIDPattern.FindAllString(copyText, -1)
		Equal(e, "copies made", len(copies), 2)
		for _, c := range copies {
			if c == ws {
				e.T.Fatal("a copy reused the original's ID")
			}
			m.ok("write_file", map[string]any{"workspace": c, "path": "app/state.txt", "content": "fix " + c})
		}
		for _, c := range copies {
			Equal(e, "copies are independent", m.ok("read_file", map[string]any{"workspace": c, "path": "app/state.txt"}), "fix "+c)
		}
		// the workspace named last stays the default
		Equal(e, "default follows the last workspace named", m.ok("read_file", map[string]any{"path": "app/state.txt"}), "fix "+copies[1])
		Equal(e, "original untouched", m.ok("read_file", map[string]any{"workspace": ws, "path": "app/state.txt"}), "one")
		m.ok("workspace_delete", map[string]any{"workspace": copies[0]})
		if list := m.ok("workspace_list", map[string]any{}); strings.Contains(list, copies[0]) || !strings.Contains(list, ws) {
			e.T.Fatalf("workspace_list after delete: %q", list)
		}

		// folders between the agent's machine and the workspace
		local := filepath.Join(e.T.TempDir(), "repo")
		e.Must(os.MkdirAll(filepath.Join(local, "pkg"), 0o755))
		e.Must(os.WriteFile(filepath.Join(local, "pkg", "lib.txt"), []byte("library"), 0o644))
		m.ok("push_dir", map[string]any{"workspace": ws, "local_dir": local, "remote_dir": "src/repo"})
		m.ok("exec", map[string]any{"command": "echo built > src/repo/pkg/out.txt"})
		back := filepath.Join(e.T.TempDir(), "back")
		m.ok("pull_dir", map[string]any{"remote_dir": "src/repo", "local_dir": back})
		got, err := os.ReadFile(filepath.Join(back, "pkg", "out.txt"))
		e.Must(err)
		Equal(e, "work pulled back", string(got), "built\n")

		// long output is cut to its start and end, small enough for an agent's context
		out = m.ok("exec", map[string]any{"command": "i=0; while [ $i -lt 4000 ]; do echo line-$i-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; i=$((i+1)); done"})
		if len(out) > 25000 || !strings.Contains(out, "line-0-") || !strings.Contains(out, "line-3999-") || !strings.Contains(out, "bytes cut") {
			e.T.Fatalf("long output: %d chars, start/end kept: %v/%v", len(out), strings.Contains(out, "line-0-"), strings.Contains(out, "line-3999-"))
		}

		// engine failures are tool errors that carry Cairn's code
		text, isErr := m.tool("read_file", map[string]any{"path": "no/such/file"})
		if !isErr || !strings.Contains(text, "not_found") {
			e.T.Fatalf("missing file: isError=%v %q", isErr, text)
		}
		text, isErr = m.tool("rewind", map[string]any{"save": "sv_0000000000000000"})
		if !isErr || !strings.Contains(text, "not_found") {
			e.T.Fatalf("unknown save: isError=%v %q", isErr, text)
		}
		if msg := m.call("tools/call", map[string]any{"name": "no_such_tool", "arguments": map[string]any{}}); msg["error"] == nil {
			e.T.Fatalf("unknown tool: %v", msg)
		}
		if msg := m.call("no/such/method", map[string]any{}); msg["error"] == nil {
			e.T.Fatalf("unknown method: %v", msg)
		}

		// the server ends cleanly when the agent closes its stdin
		e.Must(m.in.Close())
		done := make(chan error, 1)
		go func() { done <- m.cmd.Wait() }()
		select {
		case err := <-done:
			e.Must(err)
		case <-time.After(10 * time.Second):
			e.T.Fatal("mcp server kept running after stdin closed")
		}
	})

	// G10.12.2: a background server and a session survive a full save and rewind.
	register("G10.12.2", func(e *Env) {
		m := startMCP(e)
		m.initialize()
		firstMatch(e, wsIDPattern, m.ok("workspace_create", map[string]any{"name": "mcp-server"}))
		started := m.ok("exec", map[string]any{"background": true,
			"command": "echo $$ > /tmp/server.pid; n=0; while :; do n=$((n+1)); echo $n > /tmp/server.ticks; sleep 0.1; done"})
		if !strings.Contains(started, "pid") {
			e.T.Fatalf("background start: %q", started)
		}
		pid := func() string {
			e.T.Helper()
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				// not_found until the background command has started
				if p, isErr := m.tool("read_file", map[string]any{"path": "/tmp/server.pid"}); !isErr && strings.TrimSpace(p) != "" {
					return strings.TrimSpace(p)
				}
				time.Sleep(100 * time.Millisecond)
			}
			e.T.Fatal("the background server never wrote its pid")
			return ""
		}
		alive := func(p string) bool {
			text := m.ok("exec", map[string]any{"command": "kill -0 " + p + " && echo alive"})
			return strings.Contains(text, "alive")
		}
		p1 := pid()
		if !alive(p1) {
			e.T.Fatalf("the background server died once exec returned")
		}
		m.ok("session_run", map[string]any{"code": "answer = 41"})

		sv := firstMatch(e, saveIDPattern, m.ok("save", map[string]any{"full": true}))
		// save full=true carries on at once: the server and session are still there
		if !alive(p1) {
			e.T.Fatal("the server stopped after a full save")
		}
		m.ok("session_run", map[string]any{"code": "answer = 'changed'"})
		m.ok("exec", map[string]any{"command": "kill " + p1})
		if alive(p1) {
			e.T.Fatal("could not stop the server")
		}

		m.ok("rewind", map[string]any{"save": sv})
		Equal(e, "same server process after rewind", pid(), p1)
		if !alive(p1) {
			e.T.Fatal("rewinding to a full save did not bring the server back")
		}
		ticks := func() string {
			return strings.TrimSpace(m.ok("read_file", map[string]any{"path": "/tmp/server.ticks"}))
		}
		before := ticks()
		time.Sleep(500 * time.Millisecond)
		if after := ticks(); after == before {
			e.T.Fatalf("the server is not running after rewind (ticks stuck at %s)", before)
		}
		out := m.ok("session_run", map[string]any{"code": "answer + 1"})
		if !strings.Contains(out, "42") {
			e.T.Fatalf("the session's variables did not come back with the rewind: %q", out)
		}
	})

	register("G10.12.3", func(e *Env) {
		m := startMCP(e)
		m.initialize()
		ws := firstMatch(e, wsIDPattern, m.ok("workspace_create", map[string]any{"name": "recall"}))
		m.ok("exec", map[string]any{"command": "echo built > out.txt"})
		m.ok("exec", map[string]any{"command": "echo failing-step; exit 3"})
		sv := firstMatch(e, saveIDPattern, m.ok("save", map[string]any{}))
		m.ok("write_file", map[string]any{"path": "notes.md", "content": "wip"})
		m.ok("rewind", map[string]any{"save": sv})

		h := m.ok("history", map[string]any{})
		lines := strings.Split(strings.TrimSpace(h), "\n")
		find := func(what string) int {
			for i, l := range lines {
				if strings.Contains(l, what) {
					return i
				}
			}
			e.T.Fatalf("history has no line with %q:\n%s", what, h)
			return -1
		}
		failed := find("exit 3")
		if !strings.Contains(lines[failed], "failing-step") {
			e.T.Fatalf("the failing command's line does not show the command: %q", lines[failed])
		}
		saved, wrote, rewound := find(sv), find("notes.md"), find("resume")
		if !(find("echo built") < failed && failed < saved && saved < wrote && wrote < rewound) {
			e.T.Fatalf("history is not oldest first:\n%s", h)
		}

		// only commands
		for _, l := range strings.Split(strings.TrimSpace(m.ok("history", map[string]any{"kind": "exec"})), "\n")[1:] {
			if !strings.Contains(l, "exec:") {
				e.T.Fatalf("kind=exec returned %q", l)
			}
		}
		// the newest one
		if one := strings.Split(strings.TrimSpace(m.ok("history", map[string]any{"limit": 1})), "\n"); len(one) != 2 || !strings.Contains(one[1], "resume") {
			e.T.Fatalf("limit=1: %q", one)
		}

		// a new conversation, which never saw any of it
		m2 := startMCP(e)
		m2.initialize()
		if h2 := m2.ok("history", map[string]any{"workspace": ws}); !strings.Contains(h2, "exit 3") || !strings.Contains(h2, sv) {
			e.T.Fatalf("history from a new conversation:\n%s", h2)
		}
	})

	register("G10.12.4", func(e *Env) {
		m := startMCP(e)
		m.initialize()
		m.ok("workspace_create", map[string]any{"name": "review"})
		m.ok("exec", map[string]any{"command": "mkdir -p review && echo one > review/a.txt && echo bye > review/gone.txt"})
		first := firstMatch(e, saveIDPattern, m.ok("save", map[string]any{}))
		m.ok("exec", map[string]any{"command": "echo two >> review/a.txt && rm review/gone.txt && echo n > review/new.txt"})
		second := firstMatch(e, saveIDPattern, m.ok("save", map[string]any{}))

		out := m.ok("diff", map[string]any{"save": second})
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if !strings.Contains(lines[0], "from "+first+" to "+second) {
			e.T.Fatalf("summary does not name the saves compared: %q", lines[0])
		}
		for _, want := range []string{"M ", "D ", "A "} {
			found := false
			for _, l := range lines[1:] {
				suffix := map[string]string{"M ": "review/a.txt", "D ": "review/gone.txt", "A ": "review/new.txt"}[want]
				if strings.HasPrefix(l, want) && strings.HasSuffix(l, suffix) {
					found = true
				}
			}
			if !found {
				e.T.Fatalf("diff lacks the %q change:\n%s", want, out)
			}
		}
		if !strings.Contains(lines[0], "1 added, 1 modified, 1 removed") {
			e.T.Fatalf("counts: %q", lines[0])
		}

		back := m.ok("diff", map[string]any{"save": first, "from": second})
		hasLine := func(out, prefix, suffix string) bool {
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, prefix) && strings.HasSuffix(l, suffix) {
					return true
				}
			}
			return false
		}
		if !strings.Contains(back, "1 added, 1 modified, 1 removed") || !hasLine(back, "A ", "review/gone.txt") || !hasLine(back, "D ", "review/new.txt") {
			e.T.Fatalf("diff the other way:\n%s", back)
		}
		if cut := m.ok("diff", map[string]any{"save": second, "limit": 1}); !strings.Contains(cut, "first 1 listed") {
			e.T.Fatalf("limit 1 does not say the list was cut:\n%s", cut)
		}
		if _, isErr := m.tool("diff", map[string]any{}); !isErr {
			e.T.Fatal("diff without a save is not an error")
		}
	})

	register("G10.12.5", func(e *Env) {
		m := startMCP(e)
		m.initialize()
		m.ok("workspace_create", map[string]any{"name": "milestone"})
		plain := firstMatch(e, saveIDPattern, m.ok("save", map[string]any{}))
		out := m.ok("save", map[string]any{"pin": true})
		kept := firstMatch(e, saveIDPattern, out)
		if !strings.Contains(out, "pinned") {
			e.T.Fatalf("save pin=true does not say the save is pinned: %q", out)
		}
		sv, err := e.C.GetSave(e.Ctx, kept)
		e.Must(err)
		Equal(e, "pinned on the engine", sv.Pinned, true)
		list := m.ok("list_saves", map[string]any{})
		for _, l := range strings.Split(list, "\n") {
			if strings.HasPrefix(l, kept) && !strings.Contains(l, "(pinned)") {
				e.T.Fatalf("list_saves does not mark the pinned save: %q", l)
			}
			if strings.HasPrefix(l, plain) && strings.Contains(l, "(pinned)") {
				e.T.Fatalf("list_saves marks an unpinned save: %q", l)
			}
		}
	})

	register("G10.12.6", func(e *Env) {
		m := startMCP(e)
		m.initialize()
		out := m.ok("workspace_create", map[string]any{"name": "limited", "allow": []string{"pypi.org", "*.github.com"}})
		ws := firstMatch(e, wsIDPattern, out)
		if !strings.Contains(out, "only to pypi.org, *.github.com") {
			e.T.Fatalf("workspace_create does not say the network is limited: %q", out)
		}
		w, err := e.C.GetWorkspace(e.Ctx, ws)
		e.Must(err)
		if strings.Join(w.Egress, ",") != "pypi.org,*.github.com" || !w.Network {
			e.T.Fatalf("workspace egress %v network %v", w.Egress, w.Network)
		}
	})
}
