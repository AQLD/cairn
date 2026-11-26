package main

// cairn up / down / status: a local engine in Docker (Docker Desktop, OrbStack,
// colima or Docker on Linux), so a laptop gets real gVisor workspaces with
// full saves in one command. The engine runs privileged in its container
// (gVisor builds each sandbox's namespaces and cgroups); the workspaces
// themselves run on gVisor's kernel, not the container's.
//
// Only containers and volumes labelled upLabel are ever touched, so a name
// that collides with something else is refused rather than replaced.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// DefaultImage is the published engine image.
const DefaultImage = "ghcr.io/aqld/cairnd:latest"

// localImage is what `cairn up --build` tags.
const localImage = "cairnd:local"

// upLabel marks the containers and volumes cairn up made.
const upLabel = "dev.cairn.up"

// labels cairn up records on the container, so a later `up` can tell whether
// the engine already matches what was asked for.
const (
	labelImage    = "dev.cairn.image"
	labelTemplate = "dev.cairn.template"
)

type upOptions struct {
	name, image, template string
	port                  int
	build                 bool
	native                bool            // cairnd on the host, not in Docker (upnative.go)
	docker                bool            // Docker even where native would do
	data                  string          // --native: the engine's folder
	set                   map[string]bool // flags given on the command line
}

func cmdUp(ctx context.Context, args []string) (int, error) {
	o := &upOptions{set: map[string]bool{}}
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.StringVar(&o.name, "name", "cairn", "container name (its data volume is NAME-data)")
	fs.IntVar(&o.port, "port", 7420, "port on 127.0.0.1 for the engine's API")
	fs.StringVar(&o.image, "image", DefaultImage, "engine image")
	fs.BoolVar(&o.build, "build", false, "build the image from this checkout of the Cairn repository instead of pulling it")
	fs.StringVar(&o.template, "template", "python=python:3.12-slim", "NAME=IMAGE pulled on first start (or, with --native, NAME=ROOTFS.tar imported) and used for workspaces created without a template (empty: none)")
	fs.BoolVar(&o.native, "native", false, "Linux: run cairnd straight on this host (as root, gVisor's runsc on PATH) instead of in Docker (the default there, when it can)")
	fs.BoolVar(&o.docker, "docker", false, "run the engine in Docker even where it could run natively")
	fs.StringVar(&o.data, "data", "", "with --native: the engine's folder (default "+nativeRoot+"/NAME)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if o.native && o.docker {
		return 2, errors.New("--native and --docker: pick one")
	}
	if o.native || o.data != "" || (!o.docker && nativeByDefault(ctx, o)) {
		return upNative(ctx, o)
	}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	if o.template != "" {
		if n, i, ok := strings.Cut(o.template, "="); !ok || n == "" || i == "" {
			return 2, fmt.Errorf("--template: want NAME=IMAGE, got %q", o.template)
		}
	}
	if err := dockerReady(ctx); err != nil {
		return 1, err
	}

	ct, err := inspect(ctx, o.name)
	if err != nil {
		return 1, err
	}
	if ct != nil && !ct.ours() {
		return 1, fmt.Errorf("a container named %q exists and was not made by cairn up; pick another --name", o.name)
	}
	if ct != nil {
		// unless asked otherwise, keep what the engine was started with
		if !o.set["port"] && ct.port() != 0 {
			o.port = ct.port()
		}
		if !o.set["image"] && !o.build {
			o.image = ct.Config.Labels[labelImage]
		}
		if !o.set["template"] {
			o.template = ct.Config.Labels[labelTemplate]
		}
	}
	if o.build {
		if o.image, err = buildImage(ctx); err != nil {
			return 1, err
		}
	}

	started := time.Now()
	fresh := false // the container is new: wait for its first template pull
	switch {
	case ct == nil:
		token := ""
		if cfg, err := client.ReadConfig(client.ConfigPath()); err == nil && cfg.Endpoint == endpointFor(o.port) {
			token = cfg.Token // a purged engine on this port: keep the token clients have
		}
		if token == "" {
			token = newToken()
		}
		if err := runEngine(ctx, o, token); err != nil {
			return 1, err
		}
		fresh = true
	case ct.differs(o, imageID(ctx, o.image)):
		// a new image (an upgrade or a --build), port or template: replace the
		// container, keep the volume and token. Stopping pauses the workspaces;
		// the new engine wakes them.
		fmt.Fprintf(os.Stderr, "cairn: replacing engine %q (%s), keeping its workspaces\n", o.name, ct.describe(o))
		if ct.State.Running {
			if out, err := docker(ctx, "stop", "--time", "150", o.name); err != nil {
				return 1, fmt.Errorf("docker stop: %v: %s", err, out)
			}
		}
		if out, err := docker(ctx, "rm", o.name); err != nil {
			return 1, fmt.Errorf("docker rm: %v: %s", err, out)
		}
		if err := runEngine(ctx, o, ct.token()); err != nil {
			return 1, err
		}
		fresh = true
	case ct.State.Running:
		fmt.Fprintf(os.Stderr, "cairn: engine %q already running\n", o.name)
	default:
		fmt.Fprintf(os.Stderr, "cairn: starting engine %q (its workspaces wake on first use)\n", o.name)
		if out, err := docker(ctx, "start", o.name); err != nil {
			return 1, fmt.Errorf("docker start: %v: %s", err, out)
		}
	}

	if ct, err = inspect(ctx, o.name); err != nil || ct == nil {
		return 1, fmt.Errorf("engine container %q vanished: %v", o.name, err)
	}
	endpoint := endpointFor(ct.port())
	// the config always names the engine brought up last, so every tool
	// follows `cairn up --name other` without being told
	if err := client.WriteConfig(client.ConfigPath(), client.Config{Endpoint: endpoint, Token: ct.token()}); err != nil {
		return 1, fmt.Errorf("writing %s: %w", client.ConfigPath(), err)
	}
	c := client.New(endpoint, ct.token())
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
	if tname, _, ok := strings.Cut(o.template, "="); ok && fresh {
		logs := func() string {
			out, _ := docker(ctx, "logs", "--since", started.UTC().Format(time.RFC3339), o.name)
			return out
		}
		if err := waitTemplate(ctx, c, logs, tname, 10*time.Minute); err != nil {
			return 1, err
		}
	}
	fmt.Printf("Cairn is running at %s (%s, gVisor %s)\n", endpoint, o.name, info.RuntimeVersion)
	fmt.Printf("config: %s\n\n", client.ConfigPath())
	fmt.Println("connect your coding agent (Claude Code, Codex, pi, Cursor), in your project:")
	fmt.Println("  cairn setup --project")
	fmt.Println("or try it by hand:")
	fmt.Println("  ws=$(cairn create)")
	fmt.Println("  cairn exec $ws -- python3 -c 'print(\"hello from a sandbox\")'")
	fmt.Println("stop with `cairn down` (workspaces and their processes are kept)")
	return 0, nil
}

// runEngine creates and starts the engine container. On failure nothing is
// left behind but the (labelled) volume, so a retry starts clean.
func runEngine(ctx context.Context, o *upOptions, token string) error {
	vol := o.name + "-data"
	if out, err := docker(ctx, "volume", "create", "--label", upLabel+"=1", vol); err != nil {
		return fmt.Errorf("docker volume create: %v: %s", err, out)
	}
	run := []string{"run", "-d", "--name", o.name,
		"--privileged", // gVisor creates namespaces, cgroups and mounts per sandbox
		"--restart", "unless-stopped",
		// stopping pauses every workspace (a full save each) so their
		// processes come back on the next start; Docker's default 10 s
		// would cut that short
		"--stop-timeout", "150",
		"-p", fmt.Sprintf("127.0.0.1:%d:7420", o.port),
		"-v", vol + ":/var/lib/cairn",
		"-e", "CAIRN_TOKEN", // the value comes from our environment, not argv
		"--label", upLabel + "=1",
		"--label", labelImage + "=" + o.image,
		"--label", labelTemplate + "=" + o.template,
		o.image, "--shutdown-timeout", "2m"}
	if tname, _, ok := strings.Cut(o.template, "="); ok {
		run = append(run, "--pull-template", o.template, "--default-template", tname)
	}
	fmt.Fprintf(os.Stderr, "cairn: starting engine %q from %s\n", o.name, o.image)
	out, err := dockerEnv(ctx, []string{"CAIRN_TOKEN=" + token}, run...)
	if err == nil {
		return nil
	}
	_, _ = docker(ctx, "rm", "-f", o.name) // docker run may leave it "created"
	hint := ""
	if !o.build && o.image == DefaultImage {
		if _, rerr := repoRoot(); rerr == nil {
			hint = "\n(in a checkout of the repository, `cairn up --build` builds the image locally)"
		}
	}
	return fmt.Errorf("docker run: %v: %s%s", err, out, hint)
}

func cmdDown(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	name := fs.String("name", "cairn", "container name")
	purge := fs.Bool("purge", false, "also delete the container, its data volume (every workspace and save) and its client config")
	data := fs.String("data", "", "a native engine's folder (cairn up --native --data)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if dir, ok := nativeDir(*name, *data); ok {
		return downNative(ctx, dir, *purge)
	}
	if err := dockerReady(ctx); err != nil {
		return 1, err
	}
	ct, err := inspect(ctx, *name)
	if err != nil {
		return 1, err
	}
	if ct != nil && !ct.ours() {
		return 1, fmt.Errorf("container %q was not made by cairn up; not touching it", *name)
	}
	if ct != nil && ct.State.Running {
		fmt.Fprintf(os.Stderr, "cairn: stopping %q (pausing its workspaces first)\n", *name)
		if out, err := docker(ctx, "stop", "--time", "150", *name); err != nil {
			return 1, fmt.Errorf("docker stop: %v: %s", err, out)
		}
	}
	if !*purge {
		if ct == nil {
			fmt.Fprintf(os.Stderr, "cairn: no engine %q\n", *name)
		} else {
			fmt.Fprintln(os.Stderr, "cairn: stopped; `cairn up` starts it again with every workspace")
		}
		return 0, nil
	}
	if ct != nil {
		if out, err := docker(ctx, "rm", "-f", *name); err != nil {
			return 1, fmt.Errorf("docker rm: %v: %s", err, out)
		}
		if cfg, err := client.ReadConfig(client.ConfigPath()); err == nil && ct.port() != 0 && cfg.Endpoint == endpointFor(ct.port()) {
			_ = os.Remove(client.ConfigPath())
		}
	}
	vol := *name + "-data"
	if label, err := docker(ctx, "volume", "inspect", "-f", "{{index .Labels \""+upLabel+"\"}}", vol); err == nil {
		if label != "1" {
			return 1, fmt.Errorf("volume %q was not made by cairn up; not deleting it", vol)
		}
		if out, err := docker(ctx, "volume", "rm", vol); err != nil {
			return 1, fmt.Errorf("docker volume rm: %v: %s", err, out)
		}
	}
	fmt.Fprintf(os.Stderr, "cairn: removed %q and all its workspaces and saves\n", *name)
	return 0, nil
}

func cmdStatus(ctx context.Context, c *client.Client, args []string) (int, error) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	name := fs.String("name", "cairn", "container name")
	data := fs.String("data", "", "a native engine's folder (cairn up --native --data)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if dir, ok := nativeDir(*name, *data); ok {
		describeNative(dir)
	} else if dockerReady(ctx) == nil {
		if ct, _ := inspect(ctx, *name); ct != nil {
			fmt.Printf("container %s: %s since %s, image %s\n", *name, ct.State.Status, ct.State.StartedAt, ct.Config.Labels[labelImage])
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
	if st, err := c.Storage(ctx); err == nil {
		fmt.Printf("disk: workspaces %s, saves %s, templates %s; %s free of %s\n", humanBytes(st.Workspaces), humanBytes(st.Saves),
			humanBytes(st.Templates), humanBytes(st.VolumeFree), humanBytes(st.VolumeTotal))
		for i, w := range st.PerWorkspace {
			if i == 5 {
				break
			}
			fmt.Printf("  %-20s %-24s %-8s %s\n", w.ID, w.Name, w.State, humanBytes(w.Bytes))
		}
	}
	return 0, nil
}

// container is the part of `docker inspect` cairn up uses.
type container struct {
	Image string // image ID
	State struct {
		Status    string
		Running   bool
		StartedAt string
	}
	Config struct {
		Env    []string
		Labels map[string]string
	}
	HostConfig struct {
		PortBindings map[string][]struct{ HostIP, HostPort string }
	}
}

func (c *container) ours() bool { return c.Config.Labels[upLabel] == "1" }

func (c *container) token() string {
	for _, kv := range c.Config.Env {
		if v, ok := strings.CutPrefix(kv, "CAIRN_TOKEN="); ok {
			return v
		}
	}
	return ""
}

func (c *container) port() int {
	for _, b := range c.HostConfig.PortBindings["7420/tcp"] {
		if p, err := strconv.Atoi(b.HostPort); err == nil {
			return p
		}
	}
	return 0
}

// differs reports whether the container is not what o asks for. newID is the
// local ID of o.image ("" if unknown), so a rebuilt or re-pulled tag counts.
func (c *container) differs(o *upOptions, newID string) bool {
	return c.port() != o.port || c.Config.Labels[labelImage] != o.image ||
		c.Config.Labels[labelTemplate] != o.template || (newID != "" && newID != c.Image)
}

func (c *container) describe(o *upOptions) string {
	var d []string
	if c.port() != o.port {
		d = append(d, fmt.Sprintf("port %d -> %d", c.port(), o.port))
	}
	if c.Config.Labels[labelImage] != o.image {
		d = append(d, "image "+c.Config.Labels[labelImage]+" -> "+o.image)
	} else {
		d = append(d, "new build of "+o.image)
	}
	if c.Config.Labels[labelTemplate] != o.template {
		d = append(d, fmt.Sprintf("template %q -> %q", c.Config.Labels[labelTemplate], o.template))
	}
	return strings.Join(d, ", ")
}

// inspect returns the named container, or nil if there is none.
func inspect(ctx context.Context, name string) (*container, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "container", "inspect", name)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if strings.Contains(strings.ToLower(errb.String()), "no such") {
			return nil, nil
		}
		return nil, fmt.Errorf("docker inspect %s: %v: %s", name, err, strings.TrimSpace(errb.String()))
	}
	var cs []container
	if err := json.Unmarshal(out.Bytes(), &cs); err != nil || len(cs) != 1 {
		return nil, fmt.Errorf("docker inspect %s: unexpected output", name)
	}
	return &cs[0], nil
}

// imageID is the local ID of an image, or "" if it is not present locally.
func imageID(ctx context.Context, image string) string {
	id, err := docker(ctx, "image", "inspect", "-f", "{{.Id}}", image)
	if err != nil {
		return ""
	}
	return id
}

func endpointFor(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

func newToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func dockerReady(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker not found: install Docker Desktop, OrbStack or colima (macOS) or Docker Engine (Linux)")
	}
	// a hung daemon never answers: bounded, so doctor and up say so instead
	// of waiting forever
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := docker(qctx, "info", "-f", "{{.OSType}}"); err != nil {
		if qctx.Err() != nil {
			return errors.New("docker does not answer (its daemon may be hung): restart Docker and try again")
		}
		return fmt.Errorf("docker is installed but not running (%s): start Docker and try again", strings.TrimSpace(out))
	}
	return nil
}

// docker runs a docker command and returns its trimmed combined output.
func docker(ctx context.Context, args ...string) (string, error) { return dockerEnv(ctx, nil, args...) }

func dockerEnv(ctx context.Context, env []string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
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

// waitTemplate waits for a new engine's background pull of its default
// template; logs returns the engine's log lines written since it started.
func waitTemplate(ctx context.Context, c *client.Client, logs func() string, name string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	said := false
	for time.Now().Before(deadline) {
		_, err := c.GetTemplate(ctx, name)
		if err == nil {
			return nil
		}
		var apiErr *client.Error
		if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
			return fmt.Errorf("checking template %s: %w", name, err)
		}
		if line := lastLine(logs(), `"template pull failed"`); line != "" && strings.Contains(line, `"template":"`+name+`"`) {
			return fmt.Errorf("pulling template %s failed (offline?); the engine is up, retry with `cairn template pull`:\n%s", name, line)
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
