package main

// What kind of project a folder is, so `cairn setup --project` can start its
// workspace from a template that has the right tools (found by watching agents
// install Node with apt into a Python workspace) and install its dependencies
// before the agent's first command (found by watching `pytest: command not
// found` be every agent's first result).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// Project templates are the full (not -slim) images: agents reach for git,
// curl and a compiler in their first minutes (found by watching them
// apt-get install git into a slim workspace).

// stack is a kind of project: the template it needs, the image that template
// is pulled from, and how its dependencies are installed.
type stack struct {
	name     string
	template string
	image    string
	marks    []string // any of these at the project's root means this stack
	deps     []string // files whose content decides the dependencies (hashed)
	install  func(dir string) string
}

var stacks = []stack{
	{name: "node", template: "dev-node", image: "node:22", marks: []string{"package.json"},
		deps: []string{"package.json", "package-lock.json", "npm-shrinkwrap.json"},
		install: func(dir string) string {
			if exists(dir, "package-lock.json") || exists(dir, "npm-shrinkwrap.json") {
				return "npm ci --no-fund --no-audit"
			}
			return "npm install --no-fund --no-audit"
		}},
	{name: "go", template: "dev-go", image: "golang:1.26", marks: []string{"go.mod"},
		deps:    []string{"go.mod", "go.sum"},
		install: func(string) string { return "go mod download" }},
	{name: "rust", template: "dev-rust", image: "rust:1", marks: []string{"Cargo.toml"},
		deps:    []string{"Cargo.toml", "Cargo.lock"},
		install: func(string) string { return "cargo fetch" }},
	{name: "python", template: "dev-python", image: "python:3.12",
		marks: []string{"requirements.txt", "pyproject.toml", "setup.py", "Pipfile"},
		deps:  []string{"requirements.txt", "requirements-dev.txt", "pyproject.toml", "setup.py", "setup.cfg"},
		install: func(dir string) string {
			pip := "python3 -m pip install --disable-pip-version-check --root-user-action=ignore -q"
			var parts []string
			for _, f := range []string{"requirements.txt", "requirements-dev.txt"} {
				if exists(dir, f) {
					parts = append(parts, pip+" -r "+f)
				}
			}
			if len(parts) == 0 && (exists(dir, "pyproject.toml") || exists(dir, "setup.py")) {
				parts = append(parts, pip+" -e .")
			}
			return strings.Join(parts, " && ")
		}},
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// detectStack names the project's stack, or nil if nothing marks one.
func detectStack(dir string) *stack {
	for i := range stacks {
		for _, m := range stacks[i].marks {
			if exists(dir, m) {
				return &stacks[i]
			}
		}
	}
	return nil
}

// depsHash fingerprints the dependency files, so a rerun installs again only
// when they changed.
func (s *stack) depsHash(dir string) string {
	h := sha256.New()
	for _, f := range s.deps {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s %d\n", f, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// chooseTemplate decides the workspace's template: --template wins (NAME, or
// NAME=IMAGE to pull it), then the project's stack, then the engine's default
// (""). A template the engine lacks is pulled; if that fails the engine's
// default is used and the reason shown.
func chooseTemplate(ctx context.Context, c *client.Client, flagValue string, st *stack) string {
	name, image := "", ""
	switch {
	case flagValue != "":
		name, image, _ = strings.Cut(flagValue, "=")
	case st != nil:
		name, image = st.template, st.image
	default:
		// no marker: still a coding project, so the Python dev image (git,
		// curl, compilers) rather than the engine's slim default
		py := stacks[len(stacks)-1]
		name, image = py.template, py.image
	}
	if _, err := c.GetTemplate(ctx, name); err == nil {
		return name
	}
	if image == "" {
		warn("template %s does not exist (give --template %s=IMAGE to pull it); using the engine's default", name, name)
		return ""
	}
	fmt.Printf("  … pulling template %s from %s (first time only)\n", name, image)
	start := time.Now()
	if _, err := c.PullTemplate(ctx, name, client.PullTemplate{Image: image}); err != nil {
		warn("could not pull %s (%v); using the engine's default template", image, err)
		return ""
	}
	ok("template %s ready (%s)", name, time.Since(start).Round(time.Second))
	return name
}

// bootstrap installs the project's dependencies in the workspace. It reports
// the command it ran (for AGENTS.md) and whether the workspace is ready.
func bootstrap(ctx context.Context, c *client.Client, ws, remote, dir string, st *stack, isolated bool) (string, bool) {
	if st == nil {
		return "", true
	}
	cmd := st.install(dir)
	if cmd == "" {
		return "", true
	}
	if !isolated {
		warn("not installing dependencies (%s): this runtime has no isolation, so it would install onto this machine", cmd)
		return cmd, false
	}
	hashFile := filepath.Join(dir, pinDir, "deps")
	hash := st.depsHash(dir)
	if old, err := os.ReadFile(hashFile); err == nil && strings.TrimSpace(string(old)) == hash+" "+ws {
		ok("dependencies up to date (%s)", cmd)
		return cmd, true
	}
	fmt.Printf("  … installing dependencies: %s\n", cmd)
	start := time.Now()
	res, err := c.Exec(ctx, ws, client.Exec{Command: []string{"sh", "-c", cmd}, Dir: remote, TimeoutMS: (15 * time.Minute).Milliseconds()})
	switch {
	case err != nil:
		warn("installing dependencies: %v", err)
		return cmd, false
	case res.TimedOut || res.ExitCode != 0:
		tail := lastLines(string(res.Stderr)+string(res.Stdout), 8)
		warn("installing dependencies failed (exit %d); the agent can retry with `cairn run -- %s`\n%s", res.ExitCode, cmd, indent(tail, "      "))
		return cmd, false
	}
	ok("dependencies installed in %s", time.Since(start).Round(time.Second))
	_ = os.WriteFile(hashFile, []byte(hash+" "+ws+"\n"), 0o644)
	return cmd, true
}

// baseline makes the clean save `cairn resume .` returns to: dependencies
// installed, nothing else done. The workspace keeps running.
func baseline(ctx context.Context, c *client.Client, ws, dir string, full bool) {
	kind := client.QuickSave
	if full {
		kind = client.FullSave
	}
	sv, err := c.SaveWith(ctx, ws, client.SaveOptions{Kind: kind, KeepRunning: true})
	if err != nil {
		warn("could not make the baseline save: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, pinDir, "baseline"), []byte(sv.ID+"\n"), 0o644); err != nil {
		warn("recording the baseline save: %v", err)
		return
	}
	ok("baseline save %s: `cairn resume .` returns the workspace to this clean state", sv.ID)
}

// projectBaseline is the project's baseline save, for `cairn resume .`.
func projectBaseline() (string, error) {
	p, err := findProject(".")
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", errors.New(`"." means this project's baseline save, but no folder from here up has one: run ` + "`cairn setup --project`" + ` in the project`)
	}
	data, err := os.ReadFile(filepath.Join(p.Dir, pinDir, "baseline"))
	if err != nil {
		return "", errors.New("this project has no baseline save yet: run `cairn setup --project` again")
	}
	return strings.TrimSpace(string(data)), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
