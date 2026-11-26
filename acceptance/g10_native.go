package acceptance

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AQLD/cairn/client"
)

// nativeEngine is an engine started by `cairn up --native`: cairnd straight
// on the host, from a folder holding the CLI, cairnd and cairn-init as an
// install would. It has its own name, port, data folder and config, and is
// purged when the test ends.
type nativeEngine struct {
	e               *Env
	cli, name, port string
	data, config    string
	templateArg     string
	native          bool // pass --native and --data (false: rely on the defaults, by --name)
}

// where names the engine for down and status: its folder, or (defaults)
// its name.
func (ne *nativeEngine) where() []string {
	if ne.native {
		return []string{"--data", ne.data}
	}
	return []string{"--name", ne.name}
}

func startNativeEngine(e *Env) *nativeEngine { return startNativeEngineAs(e, true) }

func startNativeEngineAs(e *Env, native bool) *nativeEngine {
	e.T.Helper()
	bin := e.T.TempDir()
	for _, p := range []string{"cairn", "cairnd"} {
		cmd := exec.CommandContext(e.Ctx, "go", "build", "-o", filepath.Join(bin, p), "../cmd/"+p)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			e.T.Fatalf("building %s: %v\n%s", p, err, out)
		}
	}
	copyFile(e, testInit(e.T), filepath.Join(bin, "cairn-init"), 0o755)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	e.Must(err)
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	dir := e.T.TempDir()
	ne := &nativeEngine{e: e, cli: filepath.Join(bin, "cairn"), name: "accept", port: port,
		data: filepath.Join(dir, "engine"), config: filepath.Join(dir, "config", "config.json"),
		templateArg: "base=" + os.Getenv("CAIRN_TEST_TEMPLATE"), native: native}
	if !native {
		// the default folder, under a name of its own
		var b [4]byte
		_, _ = rand.Read(b[:])
		ne.name = "accept-" + hex.EncodeToString(b[:])
		ne.data = filepath.Join("/var/lib/cairn-up", ne.name)
	}
	if os.Getenv("CAIRN_TEST_TEMPLATE") == "" {
		e.T.Skip("needs CAIRN_TEST_TEMPLATE: a rootfs tar for the engine's default template")
	}
	e.T.Cleanup(func() {
		if out, err := ne.cairn(append([]string{"down", "--purge"}, ne.where()...)...); err != nil {
			e.T.Logf("cleanup: %v\n%s", err, out)
		}
	})
	ne.up()
	return ne
}

func copyFile(e *Env, from, to string, mode os.FileMode) {
	e.T.Helper()
	in, err := os.Open(from)
	e.Must(err)
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	e.Must(err)
	_, err = io.Copy(out, in)
	e.Must(err)
	e.Must(out.Close())
}

func (ne *nativeEngine) cairn(args ...string) (string, error) {
	cmd := exec.Command(ne.cli, args...)
	env := []string{"CAIRN_CONFIG=" + ne.config}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CAIRN_ENDPOINT=") && !strings.HasPrefix(kv, "CAIRN_TOKEN=") && !strings.HasPrefix(kv, "CAIRN_CONFIG=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (ne *nativeEngine) up(extra ...string) {
	ne.e.T.Helper()
	start := time.Now()
	args := []string{"up", "--name", ne.name, "--port", ne.port, "--template", ne.templateArg}
	if ne.native {
		args = append(args, "--native", "--data", ne.data)
	}
	out, err := ne.cairn(append(args, extra...)...)
	if err != nil {
		ne.e.T.Fatalf("cairn up --native: %v\n%s", err, out)
	}
	ne.e.T.Logf("cairn up --native took %s", time.Since(start).Round(100*time.Millisecond))
}

func (ne *nativeEngine) down() {
	ne.e.T.Helper()
	if out, err := ne.cairn(append([]string{"down"}, ne.where()...)...); err != nil {
		ne.e.T.Fatalf("cairn down: %v\n%s", err, out)
	}
}

func (ne *nativeEngine) client() *client.Client {
	ne.e.T.Helper()
	cfg, err := client.ReadConfig(ne.config)
	ne.e.Must(err)
	return client.New(cfg.Endpoint, cfg.Token)
}

// pid is the engine's process, from the pid file cairn up wrote.
func (ne *nativeEngine) pid() int {
	ne.e.T.Helper()
	b, err := os.ReadFile(filepath.Join(ne.data, "cairnd.pid"))
	ne.e.Must(err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	ne.e.Must(err)
	return pid
}

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func init() {
	register("G10.11.4", func(e *Env) {
		ne := startNativeEngine(e)
		pid := ne.pid()
		if !processAlive(pid) {
			e.T.Fatalf("engine process %d is gone after cairn up returned", pid)
		}
		exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
		e.Must(err)
		Equal(e, "engine binary: the cairnd next to the CLI", exe, filepath.Join(filepath.Dir(ne.cli), "cairnd"))

		fi, err := os.Stat(ne.config)
		e.Must(err)
		Equal(e, "config file mode (it holds the token)", fi.Mode().Perm(), os.FileMode(0o600))
		cfg, err := client.ReadConfig(ne.config)
		e.Must(err)
		Equal(e, "config endpoint", cfg.Endpoint, "http://127.0.0.1:"+ne.port)
		if len(cfg.Token) < 32 {
			e.T.Fatalf("config token %q: want a generated token", cfg.Token)
		}
		_, err = client.New(cfg.Endpoint, "").ListWorkspaces(e.Ctx)
		e.ErrCode(err, "unauthorized")
		dfi, err := os.Stat(ne.data)
		e.Must(err)
		Equal(e, "engine folder mode", dfi.Mode().Perm(), os.FileMode(0o700))

		out, err := ne.cairn("status", "--data", ne.data)
		if err != nil || !strings.Contains(out, "runtime runsc") || !strings.Contains(out, "native") {
			e.T.Fatalf("cairn status: %v\n%s", err, out)
		}
		// a second up finds it running and leaves it be
		ne.up()
		Equal(e, "engine process after a plain cairn up", ne.pid(), pid)

		c := ne.client()
		info, err := c.Info(e.Ctx)
		e.Must(err)
		Equal(e, "runtime", info.Runtime, "runsc")
		if !info.Capabilities.Isolation || !info.Capabilities.FullSave {
			e.T.Fatalf("capabilities %+v: want isolation and full saves", info.Capabilities)
		}
		ws, err := c.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "first"}) // the default template
		e.Must(err)
		cpid, n := localCounter(e, c, ws.ID)
		sv, err := c.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		_, err = c.Resume(e.Ctx, sv.ID)
		e.Must(err)
		time.Sleep(500 * time.Millisecond)
		cpid2, n2 := readLocalCounter(e, c, ws.ID)
		Equal(e, "pid after full save and resume", cpid2, cpid)
		if n2 <= n {
			e.T.Fatalf("counter %d -> %d: the process did not carry on", n, n2)
		}
	})

	register("G10.11.5", func(e *Env) {
		ne := startNativeEngine(e)
		c := ne.client()
		ws, err := c.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "kept"})
		e.Must(err)
		e.Must(c.WriteFile(e.Ctx, ws.ID, "notes.txt", []byte("half done")))
		pid, n := localCounter(e, c, ws.ID)
		engine := ne.pid()

		ne.down()
		if processAlive(engine) {
			e.T.Fatalf("engine process %d still running after cairn down", engine)
		}
		if _, err := c.ListWorkspaces(e.Ctx); err == nil {
			e.T.Fatal("engine still answers after cairn down")
		}
		ne.up()

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

	register("G10.11.6", func(e *Env) {
		// a folder cairn up did not make is never deleted
		other := filepath.Join(e.T.TempDir(), "mine")
		e.Must(os.MkdirAll(other, 0o700))
		e.Must(os.WriteFile(filepath.Join(other, "keep.txt"), []byte("mine"), 0o600))
		ne := startNativeEngine(e)
		if out, err := ne.cairn("down", "--purge", "--data", other); err == nil {
			e.T.Fatalf("cairn down --purge of a folder cairn up did not make succeeded:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(other, "keep.txt")); err != nil {
			e.T.Fatalf("a folder cairn up did not make lost its files: %v", err)
		}

		c := ne.client()
		_, err := c.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "gone"})
		e.Must(err)
		engine := ne.pid()
		if out, err := ne.cairn("down", "--purge", "--data", ne.data); err != nil {
			e.T.Fatalf("cairn down --purge: %v\n%s", err, out)
		}
		if processAlive(engine) {
			e.T.Fatal("engine still running after a purge")
		}
		if _, err := os.Stat(ne.data); !os.IsNotExist(err) {
			e.T.Fatalf("engine folder after a purge: %v", err)
		}
		if _, err := os.Stat(ne.config); !os.IsNotExist(err) {
			e.T.Fatalf("client config after a purge: %v", err)
		}
	})

	register("G10.11.7", func(e *Env) {
		ne := startNativeEngineAs(e, false) // no --native
		pid := ne.pid()
		if !processAlive(pid) {
			e.T.Fatal("a plain cairn up did not start a native engine")
		}
		out, err := ne.cairn("status", "--name", ne.name)
		if err != nil || !strings.Contains(out, "native") || !strings.Contains(out, "runtime runsc") {
			e.T.Fatalf("cairn status: %v\n%s", err, out)
		}
		ne.up() // found again with no flags
		Equal(e, "engine process after a second plain cairn up", ne.pid(), pid)
		info, err := ne.client().Info(e.Ctx)
		e.Must(err)
		if !info.Capabilities.FullSave {
			e.T.Fatalf("capabilities %+v: want full saves", info.Capabilities)
		}
	})
}
