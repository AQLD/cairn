package acceptance

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// localEngine is an engine started the way a developer starts one: the cairn
// CLI's `up`, in Docker, from the image in CAIRN_TEST_DOCKER_IMAGE. It has its
// own container name, port and config file, and is purged when the test ends.
type localEngine struct {
	e               *Env
	cli, name, port string
	config          string
}

func startLocalEngine(e *Env) *localEngine {
	e.T.Helper()
	dir := e.T.TempDir()
	cli := filepath.Join(dir, "cairn")
	if out, err := exec.CommandContext(e.Ctx, "go", "build", "-o", cli, "../cmd/cairn").CombinedOutput(); err != nil {
		e.T.Fatalf("building the cairn CLI: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	e.Must(err)
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	var b [4]byte
	_, _ = rand.Read(b[:])
	le := &localEngine{e: e, cli: cli, name: "cairn-accept-" + hex.EncodeToString(b[:]), port: port,
		config: filepath.Join(dir, "config", "config.json")}
	e.T.Cleanup(func() {
		if out, err := le.cairn("down", "--purge", "--name", le.name); err != nil {
			e.T.Logf("cleanup: %v\n%s", err, out)
		}
	})
	le.up()
	return le
}

// cairn runs the CLI against this engine's config only (never the
// developer's own ~/.config/cairn or CAIRN_ENDPOINT).
func (le *localEngine) cairn(args ...string) (string, error) {
	cmd := exec.Command(le.cli, args...)
	env := []string{"CAIRN_CONFIG=" + le.config}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CAIRN_ENDPOINT=") && !strings.HasPrefix(kv, "CAIRN_TOKEN=") && !strings.HasPrefix(kv, "CAIRN_CONFIG=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (le *localEngine) up(extra ...string) {
	le.e.T.Helper()
	start := time.Now()
	out, err := le.cairn(append([]string{"up", "--image", os.Getenv("CAIRN_TEST_DOCKER_IMAGE"), "--name", le.name, "--port", le.port}, extra...)...)
	if err != nil {
		le.e.T.Fatalf("cairn up: %v\n%s", err, out)
	}
	le.e.T.Logf("cairn up took %s", time.Since(start).Round(100*time.Millisecond))
}

func (le *localEngine) down() {
	le.e.T.Helper()
	if out, err := le.cairn("down", "--name", le.name); err != nil {
		le.e.T.Fatalf("cairn down: %v\n%s", err, out)
	}
}

func (le *localEngine) containerID() string {
	le.e.T.Helper()
	out, err := exec.Command("docker", "container", "inspect", "-f", "{{.Id}}", le.name).Output()
	le.e.Must(err)
	return strings.TrimSpace(string(out))
}

// client reads the config `cairn up` wrote, as every other tool would.
func (le *localEngine) client() *client.Client {
	le.e.T.Helper()
	cfg, err := client.ReadConfig(le.config)
	le.e.Must(err)
	return client.New(cfg.Endpoint, cfg.Token)
}

// localCounter starts a process that writes "PID N" to /tmp/c ten times a second.
func localCounter(e *Env, c *client.Client, ws string) (pid string, n int) {
	e.T.Helper()
	_, err := c.Run(e.Ctx, ws, "sh", "-c", `nohup sh -c 'n=0; while :; do n=$((n+1)); echo $$ $n > /tmp/c; sleep 0.1; done' >/dev/null 2>&1 &`)
	e.Must(err)
	time.Sleep(time.Second)
	return readLocalCounter(e, c, ws)
}

func readLocalCounter(e *Env, c *client.Client, ws string) (string, int) {
	e.T.Helper()
	out, err := c.Run(e.Ctx, ws, "cat", "/tmp/c")
	e.Must(err)
	f := strings.Fields(out)
	if len(f) != 2 {
		e.T.Fatalf("counter file holds %q", out)
	}
	n, _ := strconv.Atoi(f[1])
	return f[0], n
}

func init() {
	register("G10.11.1", func(e *Env) {
		le := startLocalEngine(e)

		fi, err := os.Stat(le.config)
		e.Must(err)
		Equal(e, "config file mode (it holds the token)", fi.Mode().Perm(), os.FileMode(0o600))
		cfg, err := client.ReadConfig(le.config)
		e.Must(err)
		Equal(e, "config endpoint", cfg.Endpoint, "http://127.0.0.1:"+le.port)
		if len(cfg.Token) < 32 {
			e.T.Fatalf("config token %q: want a generated token", cfg.Token)
		}
		// the engine requires that token
		_, err = client.New(cfg.Endpoint, "").ListWorkspaces(e.Ctx)
		e.ErrCode(err, "unauthorized")

		// the CLI finds the engine through the config alone
		out, err := le.cairn("status", "--name", le.name)
		if err != nil || !strings.Contains(out, "runtime runsc") {
			e.T.Fatalf("cairn status: %v\n%s", err, out)
		}
		out, err = le.cairn("create", "--name", "from-cli")
		e.Must(err)
		cliWS := strings.TrimSpace(out)
		if out, err = le.cairn("exec", cliWS, "--", "echo", "hi"); err != nil || strings.TrimSpace(out) != "hi" {
			e.T.Fatalf("cairn exec through the config: %v\n%s", err, out)
		}

		c := le.client()
		info, err := c.Info(e.Ctx)
		e.Must(err)
		Equal(e, "runtime", info.Runtime, "runsc")
		if !info.Capabilities.Isolation || !info.Capabilities.FullSave || !info.Capabilities.PersistentProcesses {
			e.T.Fatalf("capabilities %+v: want isolation, full saves and persistent processes", info.Capabilities)
		}
		_, err = c.GetTemplate(e.Ctx, "python")
		e.Must(err)

		// no template named: the default one, python, in gVisor
		ws, err := c.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "first"})
		e.Must(err)
		out, err = c.Run(e.Ctx, ws.ID, "python3", "-c", "import platform; print(platform.release())")
		e.Must(err)
		if !strings.Contains(out, "gvisor") {
			e.T.Fatalf("kernel %q: want gVisor's", out)
		}
		s, err := c.CreateSession(e.Ctx, ws.ID, "python")
		e.Must(err)
		_, err = c.RunInSession(e.Ctx, ws.ID, s.ID, "x = 41", time.Minute)
		e.Must(err)
		r, err := c.RunInSession(e.Ctx, ws.ID, s.ID, "x + 1", time.Minute)
		e.Must(err)
		Equal(e, "session keeps its variables", strings.TrimSpace(string(r.Stdout)), "42")

		pid, n := localCounter(e, c, ws.ID)
		sv, err := c.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		_, err = c.Resume(e.Ctx, sv.ID)
		e.Must(err)
		time.Sleep(500 * time.Millisecond)
		pid2, n2 := readLocalCounter(e, c, ws.ID)
		Equal(e, "pid after full save and resume", pid2, pid)
		if n2 <= n {
			e.T.Fatalf("counter %d -> %d: the process did not carry on", n, n2)
		}
	})

	register("G10.11.2", func(e *Env) {
		le := startLocalEngine(e)
		c := le.client()
		ws, err := c.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "kept"})
		e.Must(err)
		e.Must(c.WriteFile(e.Ctx, ws.ID, "notes.txt", []byte("half done")))
		pid, n := localCounter(e, c, ws.ID)

		le.down()
		if _, err := c.ListWorkspaces(e.Ctx); err == nil {
			e.T.Fatal("engine still answers after cairn down")
		}
		le.up()

		pid2, n2 := readLocalCounter(e, c, ws.ID)
		Equal(e, "pid after cairn down and up", pid2, pid)
		time.Sleep(500 * time.Millisecond)
		if _, n3 := readLocalCounter(e, c, ws.ID); n3 <= n2 || n2 < n {
			e.T.Fatalf("counter %d -> %d -> %d: the process did not carry on", n, n2, n3)
		}
		got, err := c.ReadFile(e.Ctx, ws.ID, "notes.txt")
		e.Must(err)
		Equal(e, "file after cairn down and up", string(got), "half done")
	})
	register("G10.11.3", func(e *Env) {
		le := startLocalEngine(e)
		c := le.client()
		ws, err := c.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "kept"})
		e.Must(err)
		pid, _ := localCounter(e, c, ws.ID)
		id1 := le.containerID()

		// same settings: nothing replaced
		le.up()
		Equal(e, "container after a plain cairn up", le.containerID(), id1)

		// a new setting (here: no default template) replaces the container
		le.up("--template", "")
		if le.containerID() == id1 {
			e.T.Fatal("cairn up with a new setting kept the old engine container")
		}
		pid2, n2 := readLocalCounter(e, c, ws.ID)
		Equal(e, "pid after the engine was replaced", pid2, pid)
		time.Sleep(500 * time.Millisecond)
		if _, n3 := readLocalCounter(e, c, ws.ID); n3 <= n2 {
			e.T.Fatalf("counter %d -> %d: the process did not carry on", n2, n3)
		}

		// names it did not make are refused, never replaced or deleted
		other := le.name + "-other"
		if out, err := exec.Command("docker", "run", "-d", "--name", other, "-v", other+"-data:/x", "busybox", "sleep", "300").CombinedOutput(); err != nil {
			e.T.Skipf("cannot start a foreign container: %v %s", err, out)
		}
		e.T.Cleanup(func() {
			_ = exec.Command("docker", "rm", "-f", other).Run()
			_ = exec.Command("docker", "volume", "rm", other+"-data").Run()
		})
		foreign := func(what string) {
			e.T.Helper()
			out, err := exec.Command("docker", "container", "inspect", "-f", "{{.State.Running}} {{.Config.Image}}", other).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "true busybox" {
				e.T.Fatalf("after %s the foreign container is %q (%v)", what, out, err)
			}
		}
		if out, err := le.cairn("up", "--name", other); err == nil {
			e.T.Fatalf("cairn up took over a container it did not make:\n%s", out)
		}
		foreign("cairn up")
		if out, err := le.cairn("down", "--purge", "--name", other); err == nil {
			e.T.Fatalf("cairn down --purge deleted a container it did not make:\n%s", out)
		}
		foreign("cairn down --purge")
		if out, err := exec.Command("docker", "volume", "inspect", other+"-data").CombinedOutput(); err != nil {
			e.T.Fatalf("foreign volume gone: %v %s", err, out)
		}
	})
}
