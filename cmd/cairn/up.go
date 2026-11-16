package main

// cairn up / down / status: a local engine in Docker (Docker Desktop, OrbStack,
// colima or Docker on Linux), so a laptop gets real gVisor workspaces with
// full saves in one command. The engine runs privileged in its container
// (gVisor builds each sandbox's namespaces and cgroups); the workspaces
// themselves run on gVisor's kernel, not the container's.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// DefaultImage is the published engine image.
const DefaultImage = "ghcr.io/aqld/cairnd:latest"

// localImage is what `cairn up --build` tags.
const localImage = "cairnd:local"

type upOptions struct {
	name, image, template string
	port                  int
	build                 bool
}

func upFlags(name string, args []string) (*upOptions, *flag.FlagSet, error) {
	o := &upOptions{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&o.name, "name", "cairn", "container name (its data volume is NAME-data)")
	fs.IntVar(&o.port, "port", 7420, "port on 127.0.0.1 for the engine's API")
	fs.StringVar(&o.image, "image", DefaultImage, "engine image")
	fs.BoolVar(&o.build, "build", false, "build the image from this checkout of the Cairn repository instead of pulling it")
	fs.StringVar(&o.template, "template", "python=python:3.12-slim", "NAME=IMAGE pulled on first start and used for workspaces created without a template (empty: none)")
	return o, fs, fs.Parse(args)
}

func cmdUp(ctx context.Context, args []string) (int, error) {
	o, _, err := upFlags("up", args)
	if err != nil {
		return 2, err
	}
	if err := dockerReady(ctx); err != nil {
		return 1, err
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", o.port)

	state := containerState(ctx, o.name)
	switch state {
	case "running":
		fmt.Fprintf(os.Stderr, "cairn: engine %q already running\n", o.name)
	case "":
		token := ""
		if cfg, err := client.ReadConfig(client.ConfigPath()); err == nil && cfg.Endpoint == endpoint {
			token = cfg.Token // keep the token clients already have
		}
		if token == "" {
			var b [32]byte
			_, _ = rand.Read(b[:])
			token = hex.EncodeToString(b[:])
		}
		image := o.image
		if o.build {
			if image, err = buildImage(ctx); err != nil {
				return 1, err
			}
		}
		run := []string{"run", "-d", "--name", o.name,
			"--privileged", // gVisor creates namespaces, cgroups and mounts per sandbox
			"--restart", "unless-stopped",
			// stopping pauses every workspace (a full save each) so their
			// processes come back on the next start; Docker's default 10 s
			// would cut that short
			"--stop-timeout", "150",
			"-p", fmt.Sprintf("127.0.0.1:%d:7420", o.port),
			"-v", o.name + "-data:/var/lib/cairn",
			"-e", "CAIRN_TOKEN=" + token,
			"--label", "dev.cairn.up=1",
			image, "--shutdown-timeout", "2m"}
		if tname, timage, ok := strings.Cut(o.template, "="); ok && tname != "" && timage != "" {
			run = append(run, "--pull-template", o.template, "--default-template", tname)
		} else if o.template != "" {
			return 2, fmt.Errorf("--template: want NAME=IMAGE, got %q", o.template)
		}
		fmt.Fprintf(os.Stderr, "cairn: starting engine %q from %s\n", o.name, image)
		if out, err := docker(ctx, run...); err != nil {
			hint := ""
			if !o.build && image == DefaultImage {
				if _, rerr := repoRoot(); rerr == nil {
					hint = "\n(in a checkout of the repository, `cairn up --build` builds the image locally)"
				}
			}
			return 1, fmt.Errorf("docker run: %v: %s%s", err, out, hint)
		}
		if err := client.WriteConfig(client.ConfigPath(), client.Config{Endpoint: endpoint, Token: token}); err != nil {
			return 1, fmt.Errorf("writing %s: %w", client.ConfigPath(), err)
		}
	default:
		fmt.Fprintf(os.Stderr, "cairn: starting stopped engine %q (its workspaces wake on first use)\n", o.name)
		if out, err := docker(ctx, "start", o.name); err != nil {
			return 1, fmt.Errorf("docker start: %v: %s", err, out)
		}
	}

	cfg, err := client.ReadConfig(client.ConfigPath())
	if err != nil || cfg.Endpoint != endpoint {
		return 1, fmt.Errorf("no config for %s in %s: run `cairn down --purge` and `cairn up` again", endpoint, client.ConfigPath())
	}
	c := client.New(endpoint, cfg.Token)
	if err := waitHealthy(ctx, endpoint, 90*time.Second); err != nil {
		logs, _ := docker(ctx, "logs", "--tail", "20", o.name)
		return 1, fmt.Errorf("%w\nengine log:\n%s", err, logs)
	}
	info, err := c.Info(ctx)
	if err != nil {
		return 1, err
	}
	if !info.Capabilities.Isolation || !info.Capabilities.FullSave {
		fmt.Fprintf(os.Stderr, "cairn: warning: runtime %s has no isolation or full saves here\n", info.Runtime)
	}
	if tname, _, ok := strings.Cut(o.template, "="); ok && tname != "" {
		if err := waitTemplate(ctx, c, o.name, tname, 10*time.Minute); err != nil {
			return 1, err
		}
	}
	fmt.Printf("Cairn is running at %s (%s, gVisor %s)\n", endpoint, o.name, info.RuntimeVersion)
	fmt.Printf("config: %s\n\n", client.ConfigPath())
	fmt.Println("try:")
	fmt.Println("  ws=$(cairn create)")
	fmt.Println("  cairn exec $ws -- python3 -c 'print(\"hello from a sandbox\")'")
	fmt.Println("  cairn save $ws --full     # memory and running processes too")
	fmt.Println("stop with `cairn down` (workspaces and their processes are kept)")
	return 0, nil
}

func cmdDown(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	name := fs.String("name", "cairn", "container name")
	purge := fs.Bool("purge", false, "also delete the container, its data volume (every workspace and save) and the client config")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if err := dockerReady(ctx); err != nil {
		return 1, err
	}
	if containerState(ctx, *name) == "running" {
		fmt.Fprintf(os.Stderr, "cairn: stopping %q (pausing its workspaces first)\n", *name)
		if out, err := docker(ctx, "stop", "--time", "150", *name); err != nil {
			return 1, fmt.Errorf("docker stop: %v: %s", err, out)
		}
	}
	if !*purge {
		fmt.Fprintln(os.Stderr, "cairn: stopped; `cairn up` starts it again with every workspace")
		return 0, nil
	}
	_, _ = docker(ctx, "rm", "-f", *name)
	if out, err := docker(ctx, "volume", "rm", *name+"-data"); err != nil && !strings.Contains(out, "no such volume") {
		return 1, fmt.Errorf("docker volume rm: %v: %s", err, out)
	}
	if cfg, err := client.ReadConfig(client.ConfigPath()); err == nil && strings.HasPrefix(cfg.Endpoint, "http://127.0.0.1:") {
		_ = os.Remove(client.ConfigPath())
	}
	fmt.Fprintf(os.Stderr, "cairn: removed %q and all its workspaces and saves\n", *name)
	return 0, nil
}

func cmdStatus(ctx context.Context, c *client.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	name := fs.String("name", "cairn", "container name")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if dockerReady(ctx) == nil {
		if state := containerState(ctx, *name); state != "" {
			fmt.Printf("container %s: %s\n", *name, state)
		}
	}
	cfg, _ := client.LoadConfig()
	info, err := c.Info(ctx)
	if err != nil {
		return 1, fmt.Errorf("engine at %s: %w", cfg.Endpoint, err)
	}
	ws, err := c.ListWorkspaces(ctx)
	if err != nil {
		return 1, err
	}
	counts := map[string]int{}
	for _, w := range ws {
		counts[string(w.State)]++
	}
	fmt.Printf("engine %s: runtime %s %s, isolation %v, full saves %v\n", cfg.Endpoint, info.Runtime, info.RuntimeVersion,
		info.Capabilities.Isolation, info.Capabilities.FullSave)
	fmt.Printf("workspaces: %d %v\n", len(ws), counts)
	return 0, nil
}

func dockerReady(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker not found: install Docker Desktop, OrbStack or colima (macOS) or Docker Engine (Linux)")
	}
	if out, err := docker(ctx, "info", "-f", "{{.OSType}}"); err != nil {
		return fmt.Errorf("docker is installed but not running (%s): start Docker and try again", strings.TrimSpace(out))
	}
	return nil
}

// containerState is the container's state (running, exited, ...), or "" if
// there is no such container.
func containerState(ctx context.Context, name string) string {
	out, err := docker(ctx, "inspect", "-f", "{{.State.Status}}", name)
	if err != nil {
		return ""
	}
	return out
}

// docker runs a docker command and returns its trimmed combined output.
func docker(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// repoRoot finds the Cairn checkout the working directory is in.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && bytes.HasPrefix(mod, []byte("module github.com/AQLD/cairn\n")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not in a checkout of github.com/AQLD/cairn")
		}
		dir = parent
	}
}

func buildImage(ctx context.Context) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", fmt.Errorf("--build: %w", err)
	}
	fmt.Fprintf(os.Stderr, "cairn: building %s from %s\n", localImage, root)
	cmd := exec.CommandContext(ctx, "docker", "build", "-f", filepath.Join(root, "deploy", "docker", "Dockerfile"), "-t", localImage, root)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build: %w", err)
	}
	return localImage, nil
}

func waitHealthy(ctx context.Context, endpoint string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/healthz", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("engine at %s did not answer within %s", endpoint, limit)
}

// waitTemplate waits for the engine's background pull of the default template.
func waitTemplate(ctx context.Context, c *client.Client, container, name string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	said := false
	for time.Now().Before(deadline) {
		if _, err := c.GetTemplate(ctx, name); err == nil {
			return nil
		}
		if logs, _ := docker(ctx, "logs", "--tail", "50", container); strings.Contains(logs, `"template pull failed"`) && strings.Contains(logs, `"template":"`+name+`"`) {
			return fmt.Errorf("pulling template %s failed (offline?); the engine is up, retry with `cairn template pull`:\n%s", name, lastLine(logs, "template pull failed"))
		}
		if !said {
			fmt.Fprintf(os.Stderr, "cairn: pulling template %s (first start only)\n", name)
			said = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("template %s not ready after %s", name, limit)
}

func lastLine(s, containing string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], containing) {
			return lines[i]
		}
	}
	return ""
}
