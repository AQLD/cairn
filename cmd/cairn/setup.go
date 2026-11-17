package main

// cairn setup: one command from nothing to a coding agent working in Cairn.
// It starts the engine if none answers, then for each coding agent found on
// this machine installs the Cairn skill (Agent Skills format: Claude Code,
// Codex, pi and others load SKILL.md files) and registers `cairn mcp` where
// the agent speaks MCP. With --project it also gives the current project a
// workspace and tells the project's agents to use it (AGENTS.md / CLAUDE.md).
// Every step is safe to repeat.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/AQLD/cairn/client"
)

//go:embed skill/SKILL.md
var skillMD []byte

// skillMarker ends every skill file setup writes. It names the version, so
// doctor can tell a stale skill, and marks the file as ours to replace.
func skillMarker() string {
	h := sha256.Sum256(skillMD)
	return "<!-- written by `cairn setup`; version " + hex.EncodeToString(h[:6]) + " -->"
}

func skillContent() []byte {
	return append(append([]byte{}, skillMD...), []byte("\n"+skillMarker()+"\n")...)
}

// codingAgent is a coding agent setup knows how to connect.
type codingAgent struct {
	name, title string
	bin         string   // command on PATH
	dirs        []string // config folders under $HOME that show it is installed
	skills      string   // skills folder under $HOME ("" if it loads none)
	mcp         bool     // speaks MCP
}

var codingAgents = []codingAgent{
	{name: "claude", title: "Claude Code", bin: "claude", dirs: []string{".claude"}, skills: ".claude/skills", mcp: true},
	{name: "codex", title: "Codex", bin: "codex", dirs: []string{".codex"}, skills: ".agents/skills", mcp: true},
	// pi has no MCP by design; it uses skills and CLIs
	{name: "pi", title: "pi", bin: "pi", dirs: []string{".pi"}, skills: ".agents/skills"},
	{name: "cursor", title: "Cursor", dirs: []string{".cursor"}, mcp: true},
}

func findAgent(name string) (codingAgent, bool) {
	for _, a := range codingAgents {
		if a.name == name {
			return a, true
		}
	}
	return codingAgent{}, false
}

// installed reports whether the agent seems to be on this machine.
func (a codingAgent) installed(home string) bool {
	if a.bin != "" {
		if _, err := exec.LookPath(a.bin); err == nil {
			return true
		}
	}
	for _, d := range a.dirs {
		if fi, err := os.Stat(filepath.Join(home, d)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

func cmdSetup(ctx context.Context, c *client.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	agentsFlag := fs.String("agents", "", "comma-separated agents to connect: claude, codex, pi, cursor (default: every one found)")
	proj := fs.Bool("project", false, "also give this project a workspace, push it in, and tell its agents to use it")
	noEngine := fs.Bool("no-engine", false, "do not start an engine if none answers")
	noNetwork := fs.Bool("no-network", false, "with --project: the workspace gets no network access")
	noInstructions := fs.Bool("no-instructions", false, "with --project: leave AGENTS.md and CLAUDE.md alone")
	template := fs.String("template", "", "with --project: the workspace's template, NAME or NAME=IMAGE (default: from the project: package.json → node, go.mod → go, Cargo.toml → rust, Python files → python)")
	noInstall := fs.Bool("no-install", false, "with --project: do not install the project's dependencies in the workspace")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 1, err
	}

	// 1. an engine
	step("engine")
	if _, err := c.Info(ctx); err != nil {
		if *noEngine {
			return 1, fmt.Errorf("no engine answers (%v) and --no-engine was given", err)
		}
		fmt.Println("  none running; starting one with `cairn up`")
		if code, err := cmdUp(ctx, nil); err != nil {
			return code, err
		}
		if c, err = client.FromConfig(); err != nil {
			return 1, err
		}
	}
	info, err := c.Info(ctx)
	if err != nil {
		return 1, err
	}
	cfg, _ := client.LoadConfig()
	ok("%s (runtime %s, full saves %v)", cfg.Endpoint, info.Runtime, info.Capabilities.FullSave)
	if !info.Capabilities.Isolation {
		warn("runtime %s has no isolation: agent code runs directly on this machine", info.Runtime)
	}

	// 2. coding agents
	step("coding agents")
	var chosen []codingAgent
	if *agentsFlag != "" {
		for _, n := range strings.Split(*agentsFlag, ",") {
			a, found := findAgent(strings.TrimSpace(n))
			if !found {
				return 2, fmt.Errorf("unknown agent %q (want claude, codex, pi or cursor)", n)
			}
			chosen = append(chosen, a)
		}
	} else {
		for _, a := range codingAgents {
			if a.installed(home) {
				chosen = append(chosen, a)
			}
		}
	}
	if len(chosen) == 0 {
		fmt.Println("  none found (looked for Claude Code, Codex, pi and Cursor)")
		fmt.Println("  any agent that can run commands can use the cairn CLI; for MCP clients see `cairn mcp install`")
	}
	written := map[string]bool{}
	for _, a := range chosen {
		var did []string
		if a.skills != "" {
			path := filepath.Join(home, a.skills, "cairn", "SKILL.md")
			if !written[path] {
				msg, err := installSkill(path)
				if err != nil {
					warn("%s: skill: %v", a.title, err)
				} else {
					written[path] = true
					did = append(did, msg)
				}
			} else {
				did = append(did, "skill shared ("+tildePath(path, home)+")")
			}
		}
		if a.mcp {
			msg, err := registerMCP(a)
			if err != nil {
				warn("%s: MCP: %v", a.title, err)
			} else {
				did = append(did, msg)
			}
		}
		ok("%s: %s", a.title, strings.Join(did, "; "))
	}

	// 3. the project
	if *proj {
		step("project")
		opts := projectOptions{network: !*noNetwork, instructions: !*noInstructions, template: *template,
			install: !*noInstall, isolated: info.Capabilities.Isolation, fullSave: info.Capabilities.FullSave}
		if err := setupProject(ctx, c, chosen, opts); err != nil {
			return 1, err
		}
	}

	fmt.Println()
	fmt.Println("Done. Start your agent and ask it, for example:")
	if *proj {
		fmt.Println(`  "Run the tests in the Cairn workspace, save, then try upgrading the dependencies."`)
	} else {
		fmt.Println(`  "Create a Cairn workspace and run this project's tests there."`)
		fmt.Println("In a repository, `cairn setup --project` gives it its own workspace.")
	}
	fmt.Println("`cairn log .` (in a project) or `cairn log WORKSPACE -f` shows what the agent runs; `cairn doctor` checks everything.")
	return 0, nil
}

// installSkill writes the Cairn skill to path unless a different skill named
// cairn, not written by setup, is already there.
func installSkill(path string) (string, error) {
	old, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(old, skillContent()):
		return "skill up to date", nil
	case err == nil && !bytes.Contains(old, []byte("written by `cairn setup`")):
		return "", fmt.Errorf("%s exists and was not written by cairn setup; left as is", path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, skillContent(), 0o644); err != nil {
		return "", err
	}
	if old == nil {
		return "skill installed", nil
	}
	return "skill updated", nil
}

// registerMCP adds `cairn mcp` to the agent's MCP servers if it is not there.
func registerMCP(a codingAgent) (string, error) {
	switch a.name {
	case "claude":
		if _, err := exec.LookPath("claude"); err != nil {
			return "", errors.New("the claude command is not on PATH; run `cairn mcp install claude --write` once it is")
		}
		if exec.Command("claude", "mcp", "get", "cairn").Run() == nil {
			return "MCP already registered", nil
		}
		out, err := exec.Command("claude", "mcp", "add", "--scope", "user", "cairn", "--", selfPath(), "mcp").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("claude mcp add: %v: %s", err, bytes.TrimSpace(out))
		}
		return "MCP registered (user scope)", nil
	case "codex", "cursor":
		home, _ := os.UserHomeDir()
		if registered, _ := mcpRegistered(a, home); registered {
			return "MCP already registered", nil
		}
		var out bytes.Buffer
		code, err := withStdout(&out, func() (int, error) { return mcpInstall([]string{a.name, "--write"}) })
		if err != nil || code != 0 {
			return "", fmt.Errorf("%v %s", err, strings.TrimSpace(out.String()))
		}
		if strings.Contains(out.String(), "already") {
			return "MCP already registered", nil
		}
		return "MCP registered", nil
	}
	return "", nil
}

// withStdout runs fn with os.Stdout going to w.
func withStdout(w *bytes.Buffer, fn func() (int, error)) (int, error) {
	r, wp, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	saved := os.Stdout
	os.Stdout = wp
	done := make(chan struct{})
	go func() { _, _ = w.ReadFrom(r); close(done) }()
	code, ferr := fn()
	os.Stdout = saved
	wp.Close()
	<-done
	return code, ferr
}

// setupProject gives the project (the repository around the current folder,
// else the folder) a workspace with the project in it, pins it, and adds the
// instructions agents read.
type projectOptions struct {
	network, instructions, install bool
	isolated, fullSave             bool // the engine's runtime
	template                       string
}

func setupProject(ctx context.Context, c *client.Client, agents []codingAgent, o projectOptions) error {
	dir := mustCwd()
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		dir = strings.TrimSpace(string(out))
	}
	name := filepath.Base(dir)
	remote := "/root/" + name

	p, err := findProject(dir)
	if err != nil {
		return err
	}
	ws := ""
	if p != nil && p.Dir == dir {
		if w, err := c.GetWorkspace(ctx, p.Workspace); err == nil {
			ws, remote = w.ID, p.Remote
			ok("workspace %s (%s), already set up", ws, w.Name)
		} else {
			warn("pinned workspace %s is gone (%v); making a new one", p.Workspace, err)
		}
	}
	kind := detectStack(dir)
	if ws == "" {
		tmpl := chooseTemplate(ctx, c, o.template, kind)
		w, err := c.CreateWorkspace(ctx, client.CreateWorkspace{Name: name, Network: o.network, Template: tmpl})
		if err != nil {
			return fmt.Errorf("creating the project's workspace: %w", err)
		}
		ws = w.ID
		ok("workspace %s (%s, template %s, network %v)", ws, w.Name, cmp.Or(w.Template, "default"), o.network)
	}
	st, err := c.PushDir(ctx, ws, dir, remote, client.PushOptions{Exclude: defaultExcludes})
	if err != nil {
		return fmt.Errorf("pushing %s: %w", dir, err)
	}
	ok("pushed %s to %s (%d files, %s)", dir, remote, st.Files, humanBytes(st.Bytes))
	if err := writePin(dir, ws, remote); err != nil {
		return err
	}
	ok("pinned in %s: `.` names this workspace (`cairn exec . -- ...`, `cairn run -- ...`)", filepath.Join(dir, pinDir))

	installCmd, ready := "", true
	if o.install {
		installCmd, ready = bootstrap(ctx, c, ws, remote, dir, kind, o.isolated)
	} else if kind != nil {
		installCmd = kind.install(dir)
	}
	if ready {
		baseline(ctx, c, ws, dir, o.fullSave)
	}

	if !o.instructions {
		return nil
	}
	block := instructionsBlock(remote, installCmd, ready && o.install && installCmd != "")
	msg, err := upsertBlock(filepath.Join(dir, "AGENTS.md"), block, true)
	if err != nil {
		return err
	}
	ok("AGENTS.md: %s", msg)
	for _, a := range agents {
		if a.name != "claude" {
			continue
		}
		// Claude Code reads CLAUDE.md; point it at AGENTS.md rather than repeat it
		path := filepath.Join(dir, "CLAUDE.md")
		old, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.WriteFile(path, []byte("@AGENTS.md\n"), 0o644); err != nil {
				return err
			}
			ok("CLAUDE.md: created, importing AGENTS.md")
		case err != nil:
			return err
		case bytes.Contains(old, []byte("@AGENTS.md")):
			ok("CLAUDE.md: already imports AGENTS.md")
		default:
			msg, err := upsertBlock(path, block, false)
			if err != nil {
				return err
			}
			ok("CLAUDE.md: %s", msg)
		}
	}
	return nil
}

const (
	blockStart = "<!-- cairn:start -->"
	blockEnd   = "<!-- cairn:end -->"
)

func instructionsBlock(remote, installCmd string, installed bool) string {
	deps := ""
	switch {
	case installCmd != "" && installed:
		deps = "- Dependencies are already installed there. After changing them: `cairn run -- " + installCmd + "`.\n"
	case installCmd != "":
		deps = "- Install dependencies first: `cairn run -- " + installCmd + "`.\n"
	}
	return blockStart + `
## Run code in the Cairn workspace

This project has a Cairn workspace: a Linux sandbox that can be saved, rewound and copied. Run builds, tests, installs and servers there, not on this machine.

- ` + "`cairn run -- COMMAND`" + ` copies local edits in and runs in ` + remote + ` (e.g. ` + "`cairn run -- pytest -q`" + `).
` + deps + `- ` + "`cairn save . --full`" + ` before risky steps and after each working milestone; ` + "`cairn saves .`" + ` then ` + "`cairn resume SAVE`" + ` to rewind instead of repairing by hand; ` + "`cairn resume .`" + ` returns to the clean baseline.
- ` + "`cairn copy SAVE --count N`" + ` to try several approaches at once.
- ` + "`cairn log .`" + ` shows what has run. The cairn skill and MCP tools have the details.
` + blockEnd + "\n"
}

var blockRE = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(blockStart) + `.*?` + regexp.QuoteMeta(blockEnd) + `\n?`)

// upsertBlock puts block into the file, replacing an earlier one, leaving
// everything else as it was. create: make the file if it is missing.
func upsertBlock(path, block string, create bool) (string, error) {
	old, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		return "created", os.WriteFile(path, []byte(block), 0o644)
	}
	if err != nil {
		return "", err
	}
	var updated []byte
	msg := "updated"
	switch {
	case blockRE.Match(old):
		updated = blockRE.ReplaceAllLiteral(old, []byte(block))
	default:
		msg = "added the Cairn section"
		sep := "\n"
		if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
			sep = "\n\n"
		}
		updated = append(append(append([]byte{}, old...), sep...), block...)
	}
	if bytes.Equal(updated, old) {
		return "up to date", nil
	}
	return msg, os.WriteFile(path, updated, 0o644)
}

func step(name string) { fmt.Printf("\n%s\n", name) }
func ok(format string, a ...any) {
	fmt.Printf("  ✓ "+format+"\n", a...)
}
func warn(format string, a ...any) {
	fmt.Printf("  ! "+format+"\n", a...)
}

func tildePath(p, home string) string {
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return p
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
