package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// maxOutput is how much command output one tool result carries. Agents' hosts
// warn past about 10k tokens and cut at 25k; the rest belongs in a file.
const maxOutput = 20000

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	run         func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error)
}

// props builds a JSON schema object. Each property is name -> [type, description].
func props(required []string, p map[string][2]string) map[string]any {
	ps := map[string]any{}
	for name, td := range p {
		ps[name] = map[string]any{"type": td[0], "description": td[1]}
	}
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": ps, "required": required}
}

const wsArg = "Workspace ID. Default: the workspace used last in this conversation."

var tools = map[string]*tool{}

func toolList() []*tool {
	out := make([]*tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return toolOrder(out[i].Name) < toolOrder(out[j].Name) })
	return out
}

var order = []string{"workspace_create", "exec", "write_file", "read_file", "push_dir", "pull_dir", "session_run",
	"save", "rewind", "copy", "list_saves", "workspace_list", "pause", "workspace_delete"}

func toolOrder(name string) int {
	for i, n := range order {
		if n == name {
			return i
		}
	}
	return len(order)
}

func register(t *tool) { tools[t.Name] = t }

func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

func init() {
	register(&tool{
		Name: "workspace_create",
		Description: "Call first. Creates an isolated Linux machine (a Cairn workspace) for this task: run installs, builds, tests and " +
			"servers here, never on the user's computer. Later calls use it by default. Network is off unless network=true " +
			"(needed for package installs and git clone).",
		InputSchema: props(nil, map[string][2]string{
			"name":      {"string", "A short name for the workspace."},
			"template":  {"string", "Template (root filesystem) to start from. Default: the engine's default template."},
			"network":   {"boolean", "Allow outbound network access (package installs, git clone). Default false."},
			"memory_mb": {"integer", "Memory limit in MB. Default: the engine's."},
		}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct {
				Name     string `json:"name"`
				Template string `json:"template"`
				Network  bool   `json:"network"`
				MemoryMB int64  `json:"memory_mb"`
			}
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			ws, err := s.c.CreateWorkspace(ctx, client.CreateWorkspace{Name: a.Name, Template: a.Template, Network: a.Network,
				Limits: client.Limits{MemoryMB: a.MemoryMB}})
			if err != nil {
				return toolResult{}, err
			}
			s.use(ws.ID)
			return text("Created workspace %s (template %q, network %v). It is now the default for other tools.", ws.ID, ws.Template, ws.Network), nil
		},
	})

	register(&tool{
		Name: "exec",
		Description: "Runs a shell command (sh -c) in the workspace and returns its exit code and output. Use it for installs, builds, " +
			"tests and git. Processes keep running between calls: set background=true for servers and watchers, then keep " +
			"calling exec against them. A non-zero exit is reported, not raised. Long output is cut to its start and end: " +
			"redirect it to a file and read_file the part you need.",
		InputSchema: props([]string{"command"}, map[string][2]string{
			"workspace":   {"string", wsArg},
			"command":     {"string", "Shell command, run with sh -c."},
			"cwd":         {"string", "Working folder inside the workspace. Default /."},
			"timeout_sec": {"integer", "Stop the command after this many seconds. Default 300."},
			"background":  {"boolean", "Start the command detached (servers, watchers) and return at once with its pid and log file."},
		}),
		run: runExec,
	})

	register(&tool{
		Name:        "write_file",
		Description: "Writes a text file in the workspace (creating folders), replacing it if it exists. For many files, push_dir is faster.",
		InputSchema: props([]string{"path", "content"}, map[string][2]string{
			"workspace": {"string", wsArg},
			"path":      {"string", "Path inside the workspace, e.g. /root/app/main.py."},
			"content":   {"string", "The whole file's content."},
		}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct{ Workspace, Path, Content string }
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			ws, err := s.workspace(a.Workspace)
			if err != nil {
				return toolResult{}, err
			}
			if err := s.c.WriteFile(ctx, ws, a.Path, []byte(a.Content)); err != nil {
				return toolResult{}, err
			}
			return text("Wrote %d bytes to %s.", len(a.Content), a.Path), nil
		},
	})

	register(&tool{
		Name:        "read_file",
		Description: "Reads a file from the workspace. Works on paused and stopped workspaces too. Long files are cut to their start and end.",
		InputSchema: props([]string{"path"}, map[string][2]string{
			"workspace": {"string", wsArg},
			"path":      {"string", "Path inside the workspace."},
		}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct{ Workspace, Path string }
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			ws, err := s.workspace(a.Workspace)
			if err != nil {
				return toolResult{}, err
			}
			data, err := s.c.ReadFile(ctx, ws, a.Path)
			if err != nil {
				return toolResult{}, err
			}
			return text("%s", clip(string(data), maxOutput, "use exec with sed -n or head/tail to read a part")), nil
		},
	})

	register(&tool{
		Name: "push_dir",
		Description: "Copies a folder from the user's machine into the workspace (default /root/<folder name>), merging with what is " +
			"there. Use it to bring the user's project in before working on it; .git is included.",
		InputSchema: map[string]any{"type": "object", "required": []string{"local_dir"}, "properties": map[string]any{
			"workspace":  map[string]any{"type": "string", "description": wsArg},
			"local_dir":  map[string]any{"type": "string", "description": "Folder on the user's machine (absolute, or relative to where the agent runs)."},
			"remote_dir": map[string]any{"type": "string", "description": "Folder in the workspace. Default /root/<name of local_dir>."},
			"exclude": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Names or paths to leave out, e.g. [\"node_modules\", \".venv\", \"*.pyc\"]."},
		}},
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct {
				Workspace string   `json:"workspace"`
				LocalDir  string   `json:"local_dir"`
				RemoteDir string   `json:"remote_dir"`
				Exclude   []string `json:"exclude"`
			}
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			ws, err := s.workspace(a.Workspace)
			if err != nil {
				return toolResult{}, err
			}
			if a.RemoteDir == "" {
				abs, err := filepath.Abs(a.LocalDir)
				if err != nil {
					return toolResult{}, err
				}
				a.RemoteDir = "/root/" + filepath.Base(abs)
			}
			st, err := s.c.PushDir(ctx, ws, a.LocalDir, a.RemoteDir, client.PushOptions{Exclude: a.Exclude})
			if err != nil {
				return toolResult{}, err
			}
			return text("Copied %s to %s in %s: %d files, %d folders, %d links, %d bytes.", a.LocalDir, a.RemoteDir, ws, st.Files, st.Dirs, st.Links, st.Bytes), nil
		},
	})

	register(&tool{
		Name: "pull_dir",
		Description: "Copies a folder from the workspace to the user's machine, merging with what is there (files of the same name are " +
			"replaced). Use it to hand finished work back to the user.",
		InputSchema: props([]string{"remote_dir", "local_dir"}, map[string][2]string{
			"workspace":  {"string", wsArg},
			"remote_dir": {"string", "Folder in the workspace."},
			"local_dir":  {"string", "Folder on the user's machine, created if missing."},
		}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct {
				Workspace string `json:"workspace"`
				RemoteDir string `json:"remote_dir"`
				LocalDir  string `json:"local_dir"`
			}
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			ws, err := s.workspace(a.Workspace)
			if err != nil {
				return toolResult{}, err
			}
			st, err := s.c.PullDir(ctx, ws, a.RemoteDir, a.LocalDir)
			if err != nil {
				return toolResult{}, err
			}
			return text("Copied %s from %s to %s: %d files, %d folders, %d links, %d bytes.", a.RemoteDir, ws, a.LocalDir, st.Files, st.Dirs, st.Links, st.Bytes), nil
		},
	})

	register(&tool{
		Name: "session_run",
		Description: "Runs code in a Python or shell interpreter that stays alive in the workspace, like a notebook: variables, imports " +
			"and loaded data carry over between calls. Use it for data exploration and anything slow to load. A final Python " +
			"expression's value is printed. Past timeout_sec the code is interrupted and the session carries on.",
		InputSchema: props([]string{"code"}, map[string][2]string{
			"workspace":   {"string", wsArg},
			"code":        {"string", "Code to run."},
			"language":    {"string", "python (default) or shell."},
			"timeout_sec": {"integer", "Interrupt the code after this many seconds. Default: the engine's."},
		}),
		run: runSession,
	})

	register(&tool{
		Name: "save",
		Description: "Saves the workspace so you can rewind to this point. Call it BEFORE anything risky or hard to undo (deleting files, " +
			"migrations, dependency upgrades, big refactors) and after each working milestone. Takes about a second. " +
			"full=true also keeps memory and running processes (servers, sessions); the workspace pauses briefly and carries on.",
		InputSchema: props(nil, map[string][2]string{
			"workspace": {"string", wsArg},
			"full":      {"boolean", "Also save memory and running processes. Default false (files only)."},
		}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct {
				Workspace string `json:"workspace"`
				Full      bool   `json:"full"`
			}
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			ws, err := s.workspace(a.Workspace)
			if err != nil {
				return toolResult{}, err
			}
			opts := client.SaveOptions{Kind: client.QuickSave}
			if a.Full {
				// keep running: saving is a checkpoint, not a shutdown
				opts = client.SaveOptions{Kind: client.FullSave, KeepRunning: true}
			}
			sv, err := s.c.SaveWith(ctx, ws, opts)
			if err != nil {
				return toolResult{}, err
			}
			return text("Saved %s as %s (%s, %s). Rewind to it with rewind save=%s; copy it to try alternatives.", ws, sv.ID, sv.Kind, size(sv.Size), sv.ID), nil
		},
	})

	register(&tool{
		Name: "rewind",
		Description: "Puts a workspace back exactly as it was at one of its saves. If a step broke things or you went down a wrong " +
			"path, rewind instead of repairing by hand. Running processes come back too when the save was full. " +
			"Later work is not lost: its saves stay listed (list_saves).",
		InputSchema: props([]string{"save"}, map[string][2]string{
			"save": {"string", "Save ID (from save or list_saves)."},
		}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct{ Save string }
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			ws, err := s.c.Resume(ctx, a.Save)
			if err != nil {
				return toolResult{}, err
			}
			s.use(ws.ID)
			return text("Workspace %s is back at save %s (%s).", ws.ID, a.Save, ws.State), nil
		},
	})

	register(&tool{
		Name: "copy",
		Description: "Makes new, independent workspaces from a save. Use it to try 2 or more approaches in parallel (one per copy, keep " +
			"the one that works) or to hand a copy of the work to a subagent. Copies of a full save keep its running processes.",
		InputSchema: props([]string{"save"}, map[string][2]string{
			"save":  {"string", "Save ID to copy."},
			"count": {"integer", "How many copies. Default 1."},
		}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct {
				Save  string `json:"save"`
				Count int    `json:"count"`
			}
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			if a.Count <= 0 {
				a.Count = 1
			}
			wss, err := s.c.Copy(ctx, a.Save, a.Count)
			if err != nil {
				return toolResult{}, err
			}
			ids := make([]string, len(wss))
			for i, w := range wss {
				ids[i] = w.ID
			}
			return text("Made %d workspace(s) from %s: %s. Pass workspace=<id> to work in each.", len(ids), a.Save, strings.Join(ids, ", ")), nil
		},
	})

	register(&tool{
		Name:        "list_saves",
		Description: "Lists a workspace's saves, newest first, with the save each came from (saves form a tree when you rewind and carry on). Use it to pick a save to rewind to or copy.",
		InputSchema: props(nil, map[string][2]string{"workspace": {"string", wsArg}}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct{ Workspace string }
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			ws, err := s.workspace(a.Workspace)
			if err != nil {
				return toolResult{}, err
			}
			w, err := s.c.GetWorkspace(ctx, ws)
			if err != nil {
				return toolResult{}, err
			}
			svs, err := s.c.ListSaves(ctx, ws)
			if err != nil {
				return toolResult{}, err
			}
			return text("%s", formatSaves(w, svs)), nil
		},
	})

	register(&tool{
		Name:        "workspace_list",
		Description: "Lists all workspaces with their state. Use it to find workspaces from earlier conversations or copies.",
		InputSchema: props(nil, nil),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			wss, err := s.c.ListWorkspaces(ctx)
			if err != nil {
				return toolResult{}, err
			}
			if len(wss) == 0 {
				return text("No workspaces. Create one with workspace_create."), nil
			}
			var b strings.Builder
			for _, w := range wss {
				fmt.Fprintf(&b, "%s  %-8s  %s  created %s", w.ID, w.State, w.Name, w.CreatedAt.Local().Format(time.DateTime))
				if w.BaseSave != "" {
					fmt.Fprintf(&b, "  at save %s", w.BaseSave)
				}
				b.WriteByte('\n')
			}
			return text("%s", b.String()), nil
		},
	})

	register(&tool{
		Name:        "pause",
		Description: "Frees a workspace's memory when you are done for now; it keeps its files and processes and wakes on the next command.",
		InputSchema: props(nil, map[string][2]string{"workspace": {"string", wsArg}}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct{ Workspace string }
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			ws, err := s.workspace(a.Workspace)
			if err != nil {
				return toolResult{}, err
			}
			w, err := s.c.PauseWorkspace(ctx, ws)
			if err != nil {
				return toolResult{}, err
			}
			return text("Workspace %s is %s.", w.ID, w.State), nil
		},
	})

	register(&tool{
		Name:        "workspace_delete",
		Description: "Deletes a workspace you no longer need (for example the losing copies after trying alternatives). Its saves are kept.",
		InputSchema: props([]string{"workspace"}, map[string][2]string{"workspace": {"string", "Workspace ID to delete."}}),
		run: func(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
			var a struct{ Workspace string }
			if err := decode(args, &a); err != nil {
				return toolResult{}, err
			}
			if a.Workspace == "" {
				return toolResult{}, errors.New("workspace is required")
			}
			if err := s.c.DeleteWorkspace(ctx, a.Workspace); err != nil {
				return toolResult{}, err
			}
			s.mu.Lock()
			if s.current == a.Workspace {
				s.current = ""
			}
			s.mu.Unlock()
			return text("Deleted workspace %s (its saves are kept).", a.Workspace), nil
		},
	})
}

func runExec(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
	var a struct {
		Workspace  string `json:"workspace"`
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
		TimeoutSec int64  `json:"timeout_sec"`
		Background bool   `json:"background"`
	}
	if err := decode(args, &a); err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(a.Command) == "" {
		return toolResult{}, errors.New("command is required")
	}
	ws, err := s.workspace(a.Workspace)
	if err != nil {
		return toolResult{}, err
	}
	if a.TimeoutSec <= 0 {
		a.TimeoutSec = 300
	}
	if a.Background {
		s.mu.Lock()
		s.bg++
		logf := fmt.Sprintf("/tmp/cairn-bg-%d-%d.log", time.Now().Unix(), s.bg)
		s.mu.Unlock()
		// detached from this command's output and stdin, so exec returns at once
		script := fmt.Sprintf("nohup sh -c %s > %s 2>&1 < /dev/null & echo $!", shellQuote(a.Command), logf)
		r, err := s.c.Exec(ctx, ws, client.Exec{Command: []string{"sh", "-c", script}, Dir: a.Cwd, TimeoutMS: 30_000})
		if err != nil {
			return toolResult{}, err
		}
		if r.ExitCode != 0 {
			return text("Could not start it (exit code %d): %s", r.ExitCode, clip(r.Stderr, maxOutput, "")), nil
		}
		return text("Started in the background: pid %s, output in %s (read it with exec \"tail -n 50 %s\").", strings.TrimSpace(r.Stdout), logf, logf), nil
	}
	r, err := s.c.Exec(ctx, ws, client.Exec{Command: []string{"sh", "-c", a.Command}, Dir: a.Cwd, TimeoutMS: a.TimeoutSec * 1000})
	if err != nil {
		return toolResult{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit code %d", r.ExitCode)
	if r.TimedOut {
		fmt.Fprintf(&b, " (stopped at the %ds timeout; for long jobs use background=true)", a.TimeoutSec)
	}
	b.WriteString(formatOutput(r.Stdout, r.Stderr))
	if r.ExitCode != 0 {
		b.WriteString(s.rewindHint(ctx, ws))
	}
	return text("%s", b.String()), nil
}

func runSession(ctx context.Context, s *Server, args json.RawMessage) (toolResult, error) {
	var a struct {
		Workspace  string `json:"workspace"`
		Code       string `json:"code"`
		Language   string `json:"language"`
		TimeoutSec int64  `json:"timeout_sec"`
	}
	if err := decode(args, &a); err != nil {
		return toolResult{}, err
	}
	if a.Language == "" {
		a.Language = "python"
	}
	ws, err := s.workspace(a.Workspace)
	if err != nil {
		return toolResult{}, err
	}
	key := ws + "/" + a.Language
	var r *client.RunResult
	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		sid := s.sessions[key]
		s.mu.Unlock()
		if sid == "" {
			ses, err := s.c.CreateSession(ctx, ws, a.Language)
			if err != nil {
				return toolResult{}, err
			}
			sid = ses.ID
			s.mu.Lock()
			s.sessions[key] = sid
			s.mu.Unlock()
		}
		r, err = s.c.RunInSession(ctx, ws, sid, a.Code, time.Duration(a.TimeoutSec)*time.Second)
		var ce *client.Error
		if errors.As(err, &ce) && ce.Code == "not_found" && attempt == 0 {
			// the session is gone (a quick-save rewind, a restart): the code never
			// ran, so start a fresh session and run it there
			s.mu.Lock()
			delete(s.sessions, key)
			s.mu.Unlock()
			continue
		}
		if err != nil {
			return toolResult{}, err
		}
		break
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit code %d", r.ExitCode)
	if r.TimedOut {
		b.WriteString(" (interrupted at the timeout; the session carries on)")
	}
	if r.Ended {
		b.WriteString(" (the session's process ended; the next call starts a fresh one)")
		s.mu.Lock()
		delete(s.sessions, key)
		s.mu.Unlock()
	}
	b.WriteString(formatOutput(r.Stdout, r.Stderr))
	return text("%s", b.String()), nil
}

// rewindHint reminds an agent whose command failed that it can rewind.
func (s *Server) rewindHint(ctx context.Context, ws string) string {
	svs, err := s.c.ListSaves(ctx, ws)
	if err != nil {
		return ""
	}
	for _, sv := range svs {
		if !sv.Auto && sv.State == "ready" {
			return fmt.Sprintf("\n\n(If this step broke the workspace, rewind save=%s goes back to the last save, from %s ago.)",
				sv.ID, time.Since(sv.CreatedAt).Round(time.Second))
		}
	}
	return ""
}

func formatOutput(stdout, stderr string) string {
	var b strings.Builder
	budget := maxOutput
	if stderr != "" && stdout != "" {
		budget = maxOutput / 2
	}
	if stdout != "" {
		b.WriteString("\n--- stdout ---\n")
		b.WriteString(clip(stdout, budget, "redirect the output to a file and read the part you need"))
	}
	if stderr != "" {
		b.WriteString("\n--- stderr ---\n")
		b.WriteString(clip(stderr, budget, "redirect the output to a file and read the part you need"))
	}
	return b.String()
}

// clip keeps the start and the end of s (the end usually holds the error),
// saying how much it left out.
func clip(s string, limit int, advice string) string {
	if len(s) <= limit {
		return s
	}
	head, tail := limit/4, limit-limit/4
	note := fmt.Sprintf("\n[... %d bytes cut", len(s)-head-tail)
	if advice != "" {
		note += "; " + advice
	}
	note += " ...]\n"
	return strings.ToValidUTF8(s[:head], "") + note + strings.ToValidUTF8(s[len(s)-tail:], "")
}

func formatSaves(w *client.Workspace, svs []client.Save) string {
	if len(svs) == 0 {
		return fmt.Sprintf("Workspace %s has no saves yet. Save before risky steps.", w.ID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Saves of %s, newest first (the workspace is now at %s):\n", w.ID, orNone(w.BaseSave))
	for _, sv := range svs {
		if sv.State == "deleted" {
			continue
		}
		fmt.Fprintf(&b, "%s  %-5s  %s  %s  from %s", sv.ID, sv.Kind, sv.CreatedAt.Local().Format(time.DateTime), size(sv.Size), orNone(sv.Parent))
		if sv.Auto {
			b.WriteString("  (automatic pause save)")
		}
		if sv.ID == w.BaseSave {
			b.WriteString("  <- current")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n>>10)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
