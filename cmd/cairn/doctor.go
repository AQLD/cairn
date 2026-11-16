package main

// cairn doctor: checks everything between a coding agent and a working
// workspace, and says how to fix each problem it finds.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AQLD/cairn/client"
)

func cmdDoctor(ctx context.Context, c *client.Client, args []string) (int, error) {
	if len(args) > 0 {
		return 2, errors.New("usage: cairn doctor")
	}
	problems := 0
	bad := func(format string, a ...any) {
		problems++
		fmt.Printf("  ✗ "+format+"\n", a...)
	}
	home, _ := os.UserHomeDir()

	step("engine")
	cfg, err := client.LoadConfig()
	if err != nil {
		bad("client config %s: %v (fix: `cairn up` rewrites it)", client.ConfigPath(), err)
	} else {
		src := client.ConfigPath()
		if os.Getenv("CAIRN_ENDPOINT") != "" {
			src = "CAIRN_ENDPOINT"
		}
		ok("endpoint %s (from %s)", cfg.Endpoint, src)
		if fi, err := os.Stat(client.ConfigPath()); err == nil && fi.Mode().Perm()&0o077 != 0 {
			bad("%s is readable by others and holds the token (fix: chmod 600 %s)", client.ConfigPath(), client.ConfigPath())
		}
	}
	info, err := c.Info(ctx)
	switch {
	case err != nil:
		var apiErr *client.Error
		if errors.As(err, &apiErr) && apiErr.Code == "unauthorized" {
			bad("the engine refused the token (fix: `cairn up` writes the right one to %s)", client.ConfigPath())
		} else if dockerReady(ctx) != nil {
			bad("no engine answers, and Docker is not available: %v (fix: start Docker, then `cairn up`)", dockerReady(ctx))
		} else {
			bad("no engine answers at %s (fix: `cairn up`)", cfg.Endpoint)
		}
	default:
		ok("runtime %s %s", info.Runtime, info.RuntimeVersion)
		if !info.Capabilities.Isolation {
			bad("runtime %s has no isolation: agent code runs on this machine (fix: `cairn up` runs gVisor in Docker)", info.Runtime)
		}
		if !info.Capabilities.FullSave {
			bad("no full saves: running processes are not kept by saves (fix: `cairn up`)")
		}
		if ts, err := c.ListTemplates(ctx); err == nil {
			if len(ts) == 0 {
				bad("no templates: workspaces start empty (fix: `cairn template pull python python:3.12-slim`)")
			} else {
				names := make([]string, len(ts))
				for i, t := range ts {
					names[i] = t.Name
				}
				ok("templates: %s", strings.Join(names, ", "))
			}
		}
	}

	step("coding agents")
	found := false
	for _, a := range codingAgents {
		if !a.installed(home) {
			continue
		}
		found = true
		var notes []string
		healthy := true
		if a.skills != "" {
			path := filepath.Join(home, a.skills, "cairn", "SKILL.md")
			data, err := os.ReadFile(path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				healthy = false
				notes = append(notes, "no skill")
			case err != nil:
				healthy = false
				notes = append(notes, "skill unreadable: "+err.Error())
			case !bytes.Equal(data, skillContent()):
				healthy = false
				notes = append(notes, "skill out of date")
			default:
				notes = append(notes, "skill ok")
			}
		}
		if a.mcp {
			if registered, how := mcpRegistered(a, home); registered {
				notes = append(notes, "MCP ok")
			} else {
				healthy = false
				notes = append(notes, "MCP not registered"+how)
			}
		}
		if healthy {
			ok("%s: %s", a.title, strings.Join(notes, ", "))
		} else {
			bad("%s: %s (fix: `cairn setup --agents %s`)", a.title, strings.Join(notes, ", "), a.name)
		}
	}
	if !found {
		fmt.Println("  none found (Claude Code, Codex, pi, Cursor); other agents can use the cairn CLI or `cairn mcp install`")
	}
	if p, err := exec.LookPath("cairn"); err != nil {
		bad("cairn is not on PATH, so agents cannot run it (fix: put %s on PATH)", filepath.Dir(selfPath()))
	} else {
		ok("cairn on PATH: %s", p)
	}

	step("project")
	p, err := findProject(".")
	switch {
	case err != nil:
		bad("%v", err)
	case p == nil:
		fmt.Println("  not in a Cairn project (`cairn setup --project` in a repository gives it a workspace)")
	default:
		if info == nil {
			bad("project %s pins workspace %s, but the engine is not answering", p.Dir, p.Workspace)
			break
		}
		w, err := c.GetWorkspace(ctx, p.Workspace)
		if err != nil {
			bad("project %s pins workspace %s, which the engine does not have: %v (fix: `cairn setup --project`)", p.Dir, p.Workspace, err)
			break
		}
		ok("project %s → workspace %s (%s, %s), code at %s", p.Dir, w.ID, w.Name, w.State, p.Remote)
		if data, err := os.ReadFile(filepath.Join(p.Dir, "AGENTS.md")); err != nil || !bytes.Contains(data, []byte(blockStart)) {
			bad("AGENTS.md has no Cairn section, so agents may not use the workspace (fix: `cairn setup --project`)")
		}
	}

	fmt.Println()
	if problems > 0 {
		fmt.Printf("%d problem(s) found\n", problems)
		return 1, nil
	}
	fmt.Println("everything looks good")
	return 0, nil
}

// mcpRegistered checks the agent's own configuration for a cairn server.
func mcpRegistered(a codingAgent, home string) (bool, string) {
	switch a.name {
	case "claude":
		if _, err := exec.LookPath("claude"); err != nil {
			return false, " (claude is not on PATH, cannot check)"
		}
		return exec.Command("claude", "mcp", "get", "cairn").Run() == nil, ""
	case "codex":
		data, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
		return bytes.Contains(data, []byte("[mcp_servers.cairn]")), ""
	case "cursor":
		data, _ := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
		return bytes.Contains(data, []byte(`"cairn"`)), ""
	}
	return false, ""
}
