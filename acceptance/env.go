// Package acceptance runs the end-goal tests in package spec against a real
// engine through the public client.
//
// By default each scenario gets its own engine, in-process, on a fresh data
// folder, using the runtime named by CAIRN_TEST_RUNTIME (host or runsc; host by
// default). Set CAIRN_ENDPOINT to run against an engine you started yourself;
// scenarios that need to control the engine are then skipped.
package acceptance

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/api"
	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/failpoint"
	"github.com/AQLD/cairn/internal/runtime"
	"github.com/AQLD/cairn/spec"
)

// Env is what a scenario gets: a client, plus control over the engine when it
// runs in-process.
type Env struct {
	T   testing.TB
	Ctx context.Context
	C   *client.Client

	Info *client.Info

	// In-process only (nil / empty with CAIRN_ENDPOINT).
	Engine     *engine.Engine
	Failpoints *failpoint.Set
	Clock      *Clock      // the engine's clock; Advance it to test retention
	Logs       *SyncBuffer // the API's structured log (JSON lines)
	DataDir    string
	token      string
	rt         runtime.Runtime
	srv        *httptest.Server
	configure  func(*engine.Config)
}

// NewEnv starts an engine for one scenario (or connects to CAIRN_ENDPOINT).
func NewEnv(t testing.TB) *Env {
	return newEnv(t, "")
}

// NewEnvWithToken starts an in-process engine that requires an API token.
func NewEnvWithToken(t testing.TB, token string) *Env {
	return newEnv(t, token)
}

func newEnv(t testing.TB, token string) *Env {
	t.Helper()
	// Follow the test's own deadline (none with -timeout 0): long gates such as
	// 10,000 save-and-resume cycles must not hit an arbitrary scenario timeout.
	ctx, cancel := context.WithCancel(context.Background())
	if dt, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		if d, has := dt.Deadline(); has {
			ctx, cancel = context.WithDeadline(context.Background(), d)
		}
	}
	t.Cleanup(cancel)

	if ep := os.Getenv("CAIRN_ENDPOINT"); ep != "" {
		e := &Env{T: t, Ctx: ctx, C: client.New(ep, os.Getenv("CAIRN_TOKEN"))}
		e.loadInfo()
		return e
	}

	dataDir := t.TempDir()
	e := &Env{T: t, Ctx: ctx, DataDir: dataDir, Failpoints: failpoint.New(), Clock: &Clock{}, Logs: &SyncBuffer{}, token: token, rt: testRuntime(t, dataDir)}
	e.start()
	t.Cleanup(e.shutdown)
	return e
}

// shutdown ends the scenario: sandboxes are stopped (they outlive the engine
// by design) and runsc's namespace mount is released, so the folder can go.
func (e *Env) shutdown() {
	if e.Engine != nil {
		_ = e.Engine.StopAll(context.Background())
	}
	e.stop()
	if e.rt != nil && e.rt.Name() == "runsc" {
		unmountRunsc(e.DataDir)
	}
}

// unmountRunsc removes the mounts runsc leaves in an engine's data folder, so
// the test can delete it.
func unmountRunsc(dataDir string) {
	for _, m := range []string{"null-netns"} {
		p := filepath.Join(dataDir, "runsc", m)
		// a sandbox still exiting can hold it for a moment; then detach lazily
		for i := 0; i < 20 && exec.Command("mountpoint", "-q", p).Run() == nil; i++ {
			if exec.Command("umount", p).Run() != nil {
				time.Sleep(100 * time.Millisecond)
			}
		}
		_ = exec.Command("umount", "-l", p).Run()
	}
}

// testRuntime picks the runtime from CAIRN_TEST_RUNTIME. With runsc, workspaces
// need a real root filesystem: set CAIRN_TEST_TEMPLATE to a rootfs tar (for
// example `docker export $(docker create python:3.12-slim)`); every engine
// imports it as template "base" and uses it by default.
func testRuntime(t testing.TB, dataDir string) runtime.Runtime {
	switch name := os.Getenv("CAIRN_TEST_RUNTIME"); name {
	case "", "host":
		return runtime.NewHost()
	case "runsc":
		bin := os.Getenv("CAIRN_RUNSC")
		if bin == "" {
			bin = "runsc"
		}
		return runtime.NewRunsc(bin, filepath.Join(dataDir, "runsc"), testInit(t))
	default:
		t.Fatalf("unknown CAIRN_TEST_RUNTIME %q", name)
		return nil
	}
}

var (
	initOnce sync.Once
	initPath string
	initErr  error
)

// testInit builds cairn-init once per test binary (or uses CAIRN_INIT).
func testInit(t testing.TB) string {
	if p := os.Getenv("CAIRN_INIT"); p != "" {
		return p
	}
	initOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cairn-init-")
		if err != nil {
			initErr = err
			return
		}
		initPath = filepath.Join(dir, "cairn-init")
		cmd := exec.Command("go", "build", "-o", initPath, "github.com/AQLD/cairn/cmd/cairn-init")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			initErr = fmt.Errorf("build cairn-init: %v: %s", err, out)
		}
	})
	if initErr != nil {
		t.Fatal(initErr)
	}
	return initPath
}

func (e *Env) start() {
	e.T.Helper()
	cfg := engine.Config{DataDir: e.DataDir, Runtime: e.rt, Failpoints: e.Failpoints, Now: e.Clock.Now}
	tmpl := os.Getenv("CAIRN_TEST_TEMPLATE")
	if tmpl != "" {
		cfg.DefaultTemplate = "base"
	}
	if e.configure != nil {
		e.configure(&cfg)
	}
	eng, err := engine.Open(e.Ctx, cfg)
	if err != nil {
		e.T.Fatalf("open engine: %v", err)
	}
	if tmpl != "" {
		if _, err := eng.GetTemplate(e.Ctx, "base"); err != nil {
			f, err := os.Open(tmpl)
			if err != nil {
				e.T.Fatalf("CAIRN_TEST_TEMPLATE: %v", err)
			}
			_, err = eng.ImportTemplate(e.Ctx, "base", f)
			f.Close()
			if err != nil {
				e.T.Fatalf("import CAIRN_TEST_TEMPLATE: %v", err)
			}
		}
	}
	e.Engine = eng
	e.srv = httptest.NewServer(api.New(eng, slog.New(slog.NewJSONHandler(e.Logs, nil)), e.token))
	e.C = client.New(e.srv.URL, e.token)
	e.loadInfo()
}

func (e *Env) stop() {
	if e.srv != nil {
		e.srv.Close()
		e.srv = nil
	}
	if e.Engine != nil {
		e.Engine.Close()
		e.Engine = nil
	}
}

// Restart stops the engine without any cleanup and opens it again on the same
// data folder, as if the process had died. Failpoints are cleared.
func (e *Env) Restart() {
	e.T.Helper()
	e.Need(spec.NeedInProcess)
	e.stop()
	e.Failpoints = failpoint.New()
	e.start()
}

// RestartWith restarts the engine (as Restart does) with its configuration
// changed by fn, which stays in effect for later restarts.
func (e *Env) RestartWith(fn func(*engine.Config)) {
	e.T.Helper()
	e.configure = fn
	e.Restart()
}

// Need skips the scenario unless the environment provides n.
func (e *Env) Need(n spec.Need) {
	e.T.Helper()
	if ok, why := e.has(n); !ok {
		e.T.Skipf("needs %s: %s", n, why)
	}
}

func (e *Env) has(n spec.Need) (bool, string) {
	caps := e.Info.Capabilities
	switch n {
	case spec.NeedIsolation:
		return caps.Isolation, "runtime " + e.Info.Runtime + " has no isolation"
	case spec.NeedFullSave:
		return caps.FullSave, "runtime " + e.Info.Runtime + " cannot save memory"
	case spec.NeedResourceLimits:
		return caps.ResourceLimits, "runtime " + e.Info.Runtime + " does not enforce limits"
	case spec.NeedDiskLimits:
		return caps.DiskLimits, "runtime " + e.Info.Runtime + " does not cap disk usage"
	case spec.NeedNetworkControl:
		return caps.NetworkControl, "runtime " + e.Info.Runtime + " cannot control networking"
	case spec.NeedPersistent:
		return caps.PersistentProcesses, "runtime " + e.Info.Runtime + " does not keep processes between commands"
	case spec.NeedInProcess:
		return e.Engine != nil, "running against CAIRN_ENDPOINT"
	case spec.NeedLinuxBench, spec.NeedObjectStorage, spec.NeedMultiServer, spec.NeedRealAgent:
		return os.Getenv("CAIRN_ENV_"+envName(n)) == "1", "set CAIRN_ENV_" + envName(n) + "=1 on an environment that provides it"
	}
	return false, "unknown need"
}

func envName(n spec.Need) string {
	b := []byte(n)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func (e *Env) loadInfo() {
	e.T.Helper()
	info, err := e.C.Info(e.Ctx)
	if err != nil {
		e.T.Fatalf("engine info: %v", err)
	}
	e.Info = info
}

// SyncBuffer is a bytes.Buffer safe for concurrent writers.
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Clock is real time plus an offset tests can move forward.
type Clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// --- helpers for scenarios ---

// Must fails the scenario on err.
func (e *Env) Must(err error) {
	e.T.Helper()
	if err != nil {
		e.T.Fatal(err)
	}
}

// Workspace creates a workspace and fails the scenario on error.
func (e *Env) Workspace(name string) *client.Workspace {
	e.T.Helper()
	ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: name})
	e.Must(err)
	return ws
}

// Write writes a file into a workspace.
func (e *Env) Write(ws, path, content string) {
	e.T.Helper()
	e.Must(e.C.WriteFile(e.Ctx, ws, path, []byte(content)))
}

// Read reads a file from a workspace.
func (e *Env) Read(ws, path string) string {
	e.T.Helper()
	data, err := e.C.ReadFile(e.Ctx, ws, path)
	e.Must(err)
	return string(data)
}

// Sh runs a shell command and returns stdout, failing on a non-zero exit.
func (e *Env) Sh(ws, script string) string {
	e.T.Helper()
	out, err := e.C.Run(e.Ctx, ws, "sh", "-c", script)
	e.Must(err)
	return out
}

// Save makes a quick save.
func (e *Env) Save(ws string) *client.Save {
	e.T.Helper()
	sv, err := e.C.Save(e.Ctx, ws, client.QuickSave)
	e.Must(err)
	return sv
}

// Resume restores a save.
func (e *Env) Resume(saveID string) *client.Workspace {
	e.T.Helper()
	ws, err := e.C.Resume(e.Ctx, saveID)
	e.Must(err)
	return ws
}

// Equal fails unless got == want.
func Equal[T comparable](e *Env, what string, got, want T) {
	e.T.Helper()
	if got != want {
		e.T.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// ErrCode fails unless err is an engine error with code.
func (e *Env) ErrCode(err error, code string) {
	e.T.Helper()
	if !client.IsCode(err, code) {
		e.T.Fatalf("want error code %q, got %v", code, err)
	}
}

// Cycles is how many save-and-resume cycles loop scenarios run: CAIRN_CYCLES,
// or a small number in -short mode. The E1 gate runs it with 10000.
func Cycles() int {
	if v, err := strconv.Atoi(os.Getenv("CAIRN_CYCLES")); err == nil && v > 0 {
		return v
	}
	if testing.Short() {
		return 10
	}
	return 100
}
