// Package mcp serves Cairn to coding agents over the Model Context Protocol:
// JSON-RPC 2.0 messages, one per line, on stdin and stdout (the stdio
// transport). Every tool call goes through the public client, so an agent can
// do exactly what the REST API allows and nothing more.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"

	"github.com/AQLD/cairn/client"
)

// Protocol versions this server speaks, newest first. A client asking for one
// of them gets it back; any other gets the newest.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// instructions are shown to the agent when it connects: they decide whether it
// reaches for these tools at all.
const instructions = `Cairn gives you isolated Linux workspaces that can be saved, rewound and copied.
Run installs, builds, tests and servers in a Cairn workspace, not on the user's machine.
Save before risky steps; rewind to a save instead of repairing a broken state by hand;
copy a save to try several approaches in parallel. Processes keep running between calls,
so start servers with exec background=true and keep using them.`

// Server is one MCP session: one agent connected over stdio.
type Server struct {
	c       *client.Client
	version string
	log     *slog.Logger

	mu       sync.Mutex
	pinned   string            // --workspace / CAIRN_WORKSPACE: always the default
	current  string            // otherwise the workspace used last
	sessions map[string]string // workspace + "/" + language -> session ID
	inflight map[string]context.CancelFunc
	bg       int // background commands started, for log names

	out   io.Writer
	outMu sync.Mutex
}

// New returns a server for the engine c talks to. workspace, if set, is the
// default workspace for every tool.
func New(c *client.Client, version, workspace string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{c: c, version: version, log: log, pinned: workspace,
		sessions: map[string]string{}, inflight: map[string]context.CancelFunc{}}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
)

// Serve reads requests from in and writes responses to out until in ends.
// Tool calls run concurrently (a long command must not block a ping), and a
// notifications/cancelled stops the call it names.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	r := bufio.NewReaderSize(in, 1<<20)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			if line[0] == '[' { // a batch (2025-03-26)
				var batch []json.RawMessage
				if json.Unmarshal(line, &batch) != nil {
					s.send(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{errParse, "parse error"}})
				}
				for _, m := range batch {
					s.dispatch(ctx, m, &wg)
				}
			} else {
				s.dispatch(ctx, line, &wg)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) dispatch(ctx context.Context, msg []byte, wg *sync.WaitGroup) {
	var req request
	if err := json.Unmarshal(msg, &req); err != nil {
		s.send(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{errParse, "parse error"}})
		return
	}
	if req.Method == "" {
		return // a response to something we never send
	}
	notification := len(req.ID) == 0 || string(req.ID) == "null"
	switch req.Method {
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		if cancel := s.inflight[string(p.RequestID)]; cancel != nil {
			cancel()
		}
		s.mu.Unlock()
		return
	case "tools/call":
		if notification {
			return
		}
		cctx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.inflight[string(req.ID)] = cancel
		s.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.inflight, string(req.ID))
				s.mu.Unlock()
				cancel()
			}()
			res, rerr := s.callTool(cctx, req.Params)
			if cctx.Err() != nil && ctx.Err() == nil {
				return // cancelled: the client wants no answer
			}
			s.reply(req.ID, res, rerr)
		}()
		return
	}
	if notification {
		return // notifications/initialized and any other: nothing to say
	}
	res, rerr := s.handle(req)
	s.reply(req.ID, res, rerr)
}

func (s *Server) reply(id json.RawMessage, res any, rerr *rpcError) {
	if rerr != nil {
		s.send(response{JSONRPC: "2.0", ID: id, Error: rerr})
		return
	}
	s.send(response{JSONRPC: "2.0", ID: id, Result: res})
}

func (s *Server) handle(req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "cairn", "version": s.version},
			"instructions":    instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolList()}, nil
	default:
		return nil, &rpcError{errMethodNotFound, "method not found: " + req.Method}
	}
}

func (s *Server) send(r response) {
	data, err := json.Marshal(r)
	if err != nil {
		data, _ = json.Marshal(response{JSONRPC: "2.0", ID: r.ID, Error: &rpcError{-32603, err.Error()}})
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(append(data, '\n'))
}

// toolResult is what tools/call returns: text for the agent, isError when the
// call itself failed (a command exiting non-zero is not a failure of the call).
type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func text(format string, args ...any) toolResult {
	return toolResult{Content: []content{{Type: "text", Text: fmt.Sprintf(format, args...)}}}
}

func failure(err error) toolResult {
	msg := err.Error()
	var ce *client.Error
	if errors.As(err, &ce) {
		msg = fmt.Sprintf("Cairn error %s: %s", ce.Code, ce.Message)
		if ce.Code == "conflict" {
			msg += " (a workspace stopped by a full save carries on with rewind save=<that save>; list_saves shows it)"
		}
		if ce.Code == "unsupported_by_runtime" {
			msg += " (this engine's runtime cannot do that; full saves and lasting processes need gVisor)"
		}
	}
	return toolResult{Content: []content{{Type: "text", Text: msg}}, IsError: true}
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{errInvalidParams, "invalid params"}
	}
	t, ok := tools[p.Name]
	if !ok {
		return nil, &rpcError{errInvalidParams, "unknown tool: " + p.Name}
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	s.log.Debug("tool call", "tool", p.Name)
	res, err := t.run(ctx, s, args)
	if err != nil {
		return failure(err), nil
	}
	return res, nil
}

// workspace picks the workspace a call is about: the argument, else the
// pinned one, else the one used last.
func (s *Server) workspace(arg string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case arg != "":
		if s.pinned == "" {
			s.current = arg
		}
		return arg, nil
	case s.pinned != "":
		return s.pinned, nil
	case s.current != "":
		return s.current, nil
	}
	return "", errors.New("no workspace yet: call workspace_create first (or pass workspace)")
}

func (s *Server) use(id string) {
	s.mu.Lock()
	if s.pinned == "" {
		s.current = id
	}
	s.mu.Unlock()
}
