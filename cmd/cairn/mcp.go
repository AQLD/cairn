package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/mcp"
)

const mcpUsage = `usage:
  cairn mcp [--workspace ID]                 serve MCP on stdin/stdout (what agents launch)
  cairn mcp install CLIENT [--project] [--write]
        print (or with --write, add) the config that registers Cairn with CLIENT:
        claude, cursor, codex, vscode, windsurf, goose
        --project: this project's config file instead of the user-wide one`

func runMCP(ctx context.Context, c *client.Client, args []string) (int, error) {
	if len(args) > 0 && args[0] == "install" {
		return mcpInstall(args[1:])
	}
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	ws := fs.String("workspace", os.Getenv("CAIRN_WORKSPACE"), "default workspace for every tool (env CAIRN_WORKSPACE)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, mcpUsage) }
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	// stdout carries the protocol and nothing else
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slog.SetDefault(log)
	return 0, mcp.New(c, version(), *ws, log).Serve(ctx, os.Stdin, os.Stdout)
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// selfPath is the command agents should launch: this binary's absolute path,
// so apps started outside a shell (without PATH) find it, unless it is a
// temporary `go run` build.
func selfPath() string {
	p, err := os.Executable()
	if err != nil || strings.Contains(p, "go-build") {
		return "cairn"
	}
	return p
}

func mcpInstall(args []string) (int, error) {
	fs := flag.NewFlagSet("mcp install", flag.ContinueOnError)
	project := fs.Bool("project", false, "write this project's config file instead of the user-wide one")
	write := fs.Bool("write", false, "add the entry instead of printing it")
	pos, err := interleaved(fs, args)
	if err != nil || len(pos) != 1 {
		return 2, errors.New(mcpUsage)
	}
	bin := selfPath()
	home, _ := os.UserHomeDir()
	stdio := map[string]any{"type": "stdio", "command": bin, "args": []string{"mcp"}}
	plain := map[string]any{"command": bin, "args": []string{"mcp"}}

	switch pos[0] {
	case "claude":
		if *project {
			return jsonEntry(".mcp.json", "mcpServers", plain, *write)
		}
		cmd := []string{"claude", "mcp", "add", "--scope", "user", "cairn", "--", bin, "mcp"}
		return runOrPrint(cmd, *write)
	case "cursor":
		file := filepath.Join(home, ".cursor", "mcp.json")
		if *project {
			file = filepath.Join(".cursor", "mcp.json")
		}
		return jsonEntry(file, "mcpServers", stdio, *write)
	case "vscode":
		// VS Code keeps user-wide servers in its profile; the project file is the one to share
		return jsonEntry(filepath.Join(".vscode", "mcp.json"), "servers", stdio, *write)
	case "codex":
		file := filepath.Join(home, ".codex", "config.toml")
		entry := fmt.Sprintf("[mcp_servers.cairn]\ncommand = %q\nargs = [\"mcp\"]\ntool_timeout_sec = 600\n", bin)
		if !*write {
			fmt.Printf("# %s\n%s", file, entry)
			return 0, nil
		}
		old, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 1, err
		}
		if strings.Contains(string(old), "[mcp_servers.cairn]") {
			fmt.Printf("%s already has [mcp_servers.cairn]; left as is\n", file)
			return 0, nil
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return 1, err
		}
		sep := ""
		if len(old) > 0 && !strings.HasSuffix(string(old), "\n") {
			sep = "\n"
		}
		if len(old) > 0 {
			sep += "\n"
		}
		if err := os.WriteFile(file, append(old, []byte(sep+entry)...), 0o644); err != nil {
			return 1, err
		}
		fmt.Printf("added cairn to %s\n", file)
		return 0, nil
	case "windsurf":
		return printOnly(filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), "mcpServers", plain, *write)
	case "goose":
		if *write {
			return 2, errors.New("goose: --write is not supported; add this to ~/.config/goose/config.yaml yourself")
		}
		fmt.Printf("# ~/.config/goose/config.yaml\nextensions:\n  cairn:\n    type: stdio\n    name: cairn\n    cmd: %s\n    args: [mcp]\n    enabled: true\n    timeout: 300\n", bin)
		return 0, nil
	}
	return 2, fmt.Errorf("unknown client %q\n%s", pos[0], mcpUsage)
}

func runOrPrint(cmd []string, write bool) (int, error) {
	if !write {
		fmt.Println(strings.Join(cmd, " "))
		return 0, nil
	}
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return 1, fmt.Errorf("%s: %w", strings.Join(cmd, " "), err)
	}
	return 0, nil
}

func printOnly(file, key string, entry map[string]any, write bool) (int, error) {
	if write {
		return 2, fmt.Errorf("--write is not supported for this client; add this to %s yourself", file)
	}
	return jsonEntry(file, key, entry, false)
}

// jsonEntry prints, or merges into file, {key: {"cairn": entry}}, keeping
// every other entry and setting in the file.
func jsonEntry(file, key string, entry map[string]any, write bool) (int, error) {
	if !write {
		data, _ := json.MarshalIndent(map[string]any{key: map[string]any{"cairn": entry}}, "", "  ")
		fmt.Printf("// %s\n%s\n", file, data)
		return 0, nil
	}
	doc := map[string]any{}
	if old, err := os.ReadFile(file); err == nil && len(strings.TrimSpace(string(old))) > 0 {
		if err := json.Unmarshal(old, &doc); err != nil {
			return 1, fmt.Errorf("%s is not plain JSON (%v); add the entry yourself", file, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 1, err
	}
	servers, _ := doc[key].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["cairn"] = entry
	doc[key] = servers
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return 1, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return 1, err
	}
	if err := os.WriteFile(file, append(data, '\n'), 0o644); err != nil {
		return 1, err
	}
	fmt.Printf("added cairn to %s\n", file)
	return 0, nil
}
