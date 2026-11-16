package main

// A project is a folder (usually a repository) tied to one workspace by
// `cairn setup --project`, which writes the workspace's ID to .cairn/workspace.
// From anywhere inside the project, "." names that workspace, so agents and
// people run `cairn exec . -- make` or `cairn run -- pytest` with no IDs.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/AQLD/cairn/client"
)

const pinDir = ".cairn"

// project is a pinned project folder.
type project struct {
	Dir       string // the folder holding .cairn
	Workspace string
	Remote    string // where the folder lives in the workspace
}

// findProject looks for .cairn/workspace in dir and its parents.
func findProject(dir string) (*project, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, pinDir, "workspace"))
		if err == nil {
			p := &project{Dir: dir, Workspace: strings.TrimSpace(string(data))}
			if r, err := os.ReadFile(filepath.Join(dir, pinDir, "remote")); err == nil {
				p.Remote = strings.TrimSpace(string(r))
			}
			if p.Remote == "" {
				p.Remote = "/root/" + filepath.Base(dir)
			}
			if p.Workspace == "" {
				return nil, fmt.Errorf("%s is empty: run `cairn setup --project` again", filepath.Join(dir, pinDir, "workspace"))
			}
			return p, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
		dir = parent
	}
}

// writePin ties dir to a workspace. The folder ignores itself, so the pin
// never ends up in the repository: workspace IDs are local to one engine.
func writePin(dir, workspace, remote string) error {
	d := filepath.Join(dir, pinDir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	for name, content := range map[string]string{".gitignore": "*\n", "workspace": workspace + "\n", "remote": remote + "\n"} {
		if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// workspaceFirst lists commands whose first positional argument is a workspace.
var workspaceFirst = map[string]bool{
	"exec": true, "put": true, "get": true, "push": true, "pull": true, "save": true, "saves": true,
	"pause": true, "rm": true, "log": true,
}

// resolveDot replaces a "." in the workspace position with the project's
// workspace (CAIRN_WORKSPACE wins, as it does for `cairn mcp`).
func resolveDot(cmd string, args []string) ([]string, error) {
	pos := -1
	switch {
	case workspaceFirst[cmd]:
		for i, a := range args {
			if a == "--" {
				break
			}
			if !strings.HasPrefix(a, "-") {
				pos = i
				break
			}
		}
	case cmd == "session" && len(args) > 1:
		pos = 1
	}
	if pos < 0 || args[pos] != "." {
		return args, nil
	}
	ws, err := projectWorkspace()
	if err != nil {
		return nil, err
	}
	out := append([]string{}, args...)
	out[pos] = ws
	return out, nil
}

func projectWorkspace() (string, error) {
	if ws := os.Getenv("CAIRN_WORKSPACE"); ws != "" {
		return ws, nil
	}
	p, err := findProject(".")
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", errors.New(`"." means this project's workspace, but no folder from here up has one: run ` + "`cairn setup --project`" + ` in the project`)
	}
	return p.Workspace, nil
}

// defaultExcludes are left out when a project is pushed: rebuilt inside the
// workspace, platform-specific, or large and useless there.
var defaultExcludes = []string{"node_modules", ".venv", "venv", "__pycache__", ".cairn", ".DS_Store", ".pytest_cache", ".mypy_cache", ".next", "target"}

// cairn run [--no-push] [--exclude P]... -- COMMAND [ARGS...]
func cmdRun(ctx context.Context, c *client.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	noPush := fs.Bool("no-push", false, "run without copying local changes in first")
	var exclude stringList
	fs.Var(&exclude, "exclude", "also leave out names or paths matching this pattern when pushing (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	cmd := fs.Args()
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		return 2, errors.New("usage: cairn run [--no-push] [--exclude PATTERN]... -- COMMAND [ARGS...]")
	}
	p, err := findProject(".")
	if err != nil {
		return 1, err
	}
	if p == nil {
		return 1, errors.New("not in a Cairn project: run `cairn setup --project` in the project folder first")
	}
	if !*noPush {
		if _, err := c.PushDir(ctx, p.Workspace, p.Dir, p.Remote, client.PushOptions{Exclude: append(append([]string{}, defaultExcludes...), exclude...)}); err != nil {
			return 1, fmt.Errorf("pushing %s: %w", p.Dir, err)
		}
	}
	// the command runs in the same place relative to the project as here
	rel, err := filepath.Rel(p.Dir, mustCwd())
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = "."
	}
	dir := p.Remote
	if rel != "." {
		dir = p.Remote + "/" + filepath.ToSlash(rel)
	}
	return execStream(ctx, c, p.Workspace, client.Exec{Command: cmd, Dir: dir})
}

func mustCwd() string {
	d, _ := os.Getwd()
	return d
}
