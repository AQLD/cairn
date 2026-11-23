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
	"time"

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
	case cmd == "resume" && len(args) == 1 && args[0] == ".":
		sv, err := projectBaseline()
		if err != nil {
			return nil, err
		}
		return []string{sv}, nil
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
	noPull := fs.Bool("no-pull", false, "leave files the command wrote in the workspace instead of copying them back")
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
	// mark the moment before the command, so what it writes can be found.
	// Change times come from a coarse clock (a few ms per tick): the pauses
	// keep the push's writes before the mark's tick and the command's after
	// it, or a write in the same tick as the mark would be missed.
	mark := fmt.Sprintf("/tmp/.cairn-run-%d", time.Now().UnixNano())
	if !*noPull {
		if _, err := c.Exec(ctx, p.Workspace, client.Exec{Command: []string{"sh", "-c", `sleep 0.03; touch "$0" && sleep 0.03`, mark}}); err != nil {
			return 1, err
		}
	}
	code, err := execStream(ctx, c, p.Workspace, client.Exec{Command: cmd, Dir: dir})
	if err != nil || *noPull {
		return code, err
	}
	if perr := pullChanged(ctx, c, p, mark, append(append([]string{}, defaultExcludes...), exclude...)); perr != nil {
		fmt.Fprintln(os.Stderr, "cairn: copying results back:", perr)
	}
	return code, nil
}

// Limits on what `cairn run` copies back after a command: results such as a
// report or a build artefact, not a whole generated tree.
const (
	pullMaxFiles = 500
	pullMaxBytes = 200 << 20
)

// pullChanged copies files the command created or changed under the
// project's folder in the workspace back into the local project, so the agent
// (which reads local files) sees its results. Agents otherwise wrote
// report.md in the workspace and could not find it.
func pullChanged(ctx context.Context, c *client.Client, p *project, mark string, exclude []string) error {
	var prune []string
	for _, x := range exclude {
		prune = append(prune, "-name '"+strings.ReplaceAll(x, "'", "")+"'")
	}
	// Change time, not modification time: pushed files keep the local
	// machine's mtimes, which can be ahead of the workspace's clock, but their
	// ctime is when the push wrote them, before the mark. Relative to the
	// project folder, so it means the same on every runtime.
	script := fmt.Sprintf(`find . \( %s \) -prune -o -type f -cnewer %s -print 2>/dev/null | head -n %d; rm -f %s`,
		strings.Join(prune, " -o "), mark, pullMaxFiles+1, mark)
	res, err := c.Exec(ctx, p.Workspace, client.Exec{Command: []string{"sh", "-c", script}, Dir: p.Remote})
	if err != nil {
		return err
	}
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		if rel, ok := strings.CutPrefix(f, "./"); ok && rel != "" {
			files = append(files, rel)
		}
	}
	if len(files) == 0 {
		return nil
	}
	if len(files) > pullMaxFiles {
		fmt.Fprintf(os.Stderr, "cairn: the command changed more than %d files; copy what you need with `cairn pull . %s DIR`\n", pullMaxFiles, p.Remote)
		return nil
	}
	var total int64
	var copied []string
	for _, rel := range files {
		local := filepath.Join(p.Dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(local, p.Dir+string(filepath.Separator)) {
			continue // a name with ".." in it: never outside the project
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		tmp := local + ".cairn-tmp"
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		cw := &countingWriter{w: f}
		err = c.ReadFileTo(ctx, p.Workspace, p.Remote+"/"+rel, cw)
		f.Close()
		if err != nil {
			os.Remove(tmp)
			return fmt.Errorf("%s: %w", rel, err)
		}
		if err := os.Rename(tmp, local); err != nil {
			return err
		}
		copied = append(copied, rel)
		if total += cw.n; total > pullMaxBytes {
			fmt.Fprintf(os.Stderr, "cairn: stopped copying back after %s; copy the rest with `cairn pull`\n", humanBytes(total))
			break
		}
	}
	shown := copied
	if len(shown) > 5 {
		shown = append(append([]string{}, shown[:5]...), fmt.Sprintf("and %d more", len(copied)-5))
	}
	fmt.Fprintf(os.Stderr, "cairn: copied back %d changed file(s): %s\n", len(copied), strings.Join(shown, ", "))
	return nil
}

type countingWriter struct {
	w interface{ Write([]byte) (int, error) }
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func mustCwd() string {
	d, _ := os.Getwd()
	return d
}
