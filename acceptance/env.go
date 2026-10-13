// Package acceptance runs the end-goal tests in package spec against a real
// engine through the public client.
//
// By default each scenario gets its own engine, in-process, on a fresh data
// folder, using the runtime named by CAIRN_TEST_RUNTIME (host or runsc; host by
// default). Set CAIRN_ENDPOINT to run against an engine you started yourself;
// scenarios that need to control the engine are then skipped.
package acceptance

import (
	"context"
	"net/http/httptest"
	"os"
	"strconv"
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
	DataDir    string
	token      string
	rt         runtime.Runtime
	srv        *httptest.Server
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	if ep := os.Getenv("CAIRN_ENDPOINT"); ep != "" {
		e := &Env{T: t, Ctx: ctx, C: client.New(ep, os.Getenv("CAIRN_TOKEN"))}
		e.loadInfo()
		return e
	}

	e := &Env{T: t, Ctx: ctx, DataDir: t.TempDir(), Failpoints: failpoint.New(), token: token, rt: testRuntime(t)}
	e.start()
	t.Cleanup(e.stop)
	return e
}

func testRuntime(t testing.TB) runtime.Runtime {
	switch name := os.Getenv("CAIRN_TEST_RUNTIME"); name {
	case "", "host":
		return runtime.NewHost()
	case "runsc":
		bin := os.Getenv("CAIRN_RUNSC")
		if bin == "" {
			bin = "runsc"
		}
		return runtime.NewRunsc(bin)
	default:
		t.Fatalf("unknown CAIRN_TEST_RUNTIME %q", name)
		return nil
	}
}

func (e *Env) start() {
	e.T.Helper()
	eng, err := engine.Open(e.Ctx, engine.Config{DataDir: e.DataDir, Runtime: e.rt, Failpoints: e.Failpoints})
	if err != nil {
		e.T.Fatalf("open engine: %v", err)
	}
	e.Engine = eng
	e.srv = httptest.NewServer(api.New(eng, nil, e.token))
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
