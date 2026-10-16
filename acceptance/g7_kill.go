package acceptance

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AQLD/cairn/client"
)

// G7.6: kill -9 of the engine mid-operation recovers.
//
// The engine runs as a real cairnd process. A workload fires random operations
// at it while a killer sends SIGKILL at a random moment. After every restart,
// everything the engine acknowledged must be exactly as acknowledged, and
// nothing may be left half done.

func init() { register("G7.6", killNine) }

// KillRounds is how many kill-and-restart rounds G7.6 runs: CAIRN_KILL_ROUNDS,
// or fewer in -short mode.
func KillRounds() int {
	if v, err := strconv.Atoi(os.Getenv("CAIRN_KILL_ROUNDS")); err == nil && v > 0 {
		return v
	}
	if testing.Short() {
		return 4
	}
	return 15
}

// ledger is what the engine has acknowledged, i.e. what must survive.
type ledger struct {
	mu sync.Mutex
	// files acknowledged per live workspace: path -> content
	files map[string]map[string]string
	// saves acknowledged: save id -> its workspace and the files it holds
	saves   map[string]savedState
	deleted map[string]bool // saves acknowledged as deleted (and not undeleted)
	seq     int

	// inFlight is the operation that was running when the engine was killed.
	// Its reply was lost, but it may still have taken effect, so after a
	// restart either outcome is accepted (and the ledger follows what happened).
	// Every other operation must be exactly as acknowledged.
	inFlight *pendingOp
}

type pendingOp struct {
	kind      string // create, save, copy (retried with their key); resume, delete, undelete
	key       string // idempotency key for create, save and copy
	save      string
	workspace string
	count     int
	files     map[string]string // what a save in flight would hold
}

type savedState struct {
	workspace string
	files     map[string]string
}

func copyFiles(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func killNine(e *Env) {
	bin := buildCairnd(e)
	data := e.T.TempDir()
	e.T.Cleanup(func() { unmountRunsc(data) }) // runs before TempDir's removal
	l := &ledger{files: map[string]map[string]string{}, saves: map[string]savedState{}, deleted: map[string]bool{}}
	seed := uint64(time.Now().UnixNano())
	rng := rand.New(rand.NewPCG(seed, 7))      // the workload's choices
	killRng := rand.New(rand.NewPCG(seed, 13)) // when to kill (separate: used concurrently)
	e.T.Logf("kill rounds: %d", KillRounds())

	for round := 0; round < KillRounds(); round++ {
		proc, c := startCairnd(e, bin, data)
		verifyAfterRestart(e, c, data, l, round)

		ctx, stop := context.WithCancel(e.Ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			runRandomOps(ctx, c, l, rng, e.Info.Capabilities.FullSave)
		}()
		// kill somewhere inside an operation: sandbox operations take seconds
		window := 400
		if os.Getenv("CAIRN_TEST_RUNTIME") == "runsc" {
			window = 4000
		}
		time.Sleep(time.Duration(30+killRng.IntN(window)) * time.Millisecond)
		_ = proc.Process.Kill() // SIGKILL: no cleanup, no deferred functions
		_ = proc.Wait()
		stop()
		<-done
	}

	// a final clean start must see everything
	proc, c := startCairnd(e, bin, data)
	defer func() { _ = proc.Process.Kill(); _ = proc.Wait() }()
	verifyAfterRestart(e, c, data, l, KillRounds())
	// sandboxes outlive cairnd: stop them
	defer func() {
		ws, _ := c.ListWorkspaces(e.Ctx)
		for _, w := range ws {
			_ = c.DeleteWorkspace(e.Ctx, w.ID)
		}
	}()
	l.mu.Lock()
	e.T.Logf("acknowledged: %d workspaces, %d saves (%d deleted)", len(l.files), len(l.saves), len(l.deleted))
	l.mu.Unlock()
}

// runRandomOps performs operations one at a time and records each in the ledger
// only once the engine has acknowledged it. It stops at the first error, which
// is how it notices the kill.
func runRandomOps(ctx context.Context, c *client.Client, l *ledger, rng *rand.Rand, pausable bool) {
	for ctx.Err() == nil {
		l.mu.Lock()
		ws := keys(l.files)
		live := make([]string, 0, len(l.saves))
		for id := range l.saves {
			if !l.deleted[id] {
				live = append(live, id)
			}
		}
		dead := keysBool(l.deleted)
		l.seq++
		n := l.seq
		l.mu.Unlock()

		var err error
		switch op := rng.IntN(100); {
		case len(ws) == 0 || op < 8:
			key := fmt.Sprintf("create-%d", n)
			l.setInFlight(&pendingOp{kind: "create", key: key})
			var w *client.Workspace
			if w, err = c.CreateWorkspace(client.WithIdempotencyKey(ctx, key), client.CreateWorkspace{}); err == nil {
				l.mu.Lock()
				l.files[w.ID] = map[string]string{}
				l.inFlight = nil
				l.mu.Unlock()
			}
		case op < 45:
			id := ws[rng.IntN(len(ws))]
			path, content := fmt.Sprintf("f/%d.txt", n), fmt.Sprintf("content %d", n)
			if err = c.WriteFile(ctx, id, path, []byte(content)); err == nil {
				l.mu.Lock()
				if l.files[id] != nil {
					l.files[id][path] = content
				}
				l.mu.Unlock()
			}
		case op < 70:
			id := ws[rng.IntN(len(ws))]
			key := fmt.Sprintf("save-%d", n)
			l.mu.Lock()
			snapshot := copyFiles(l.files[id])
			l.mu.Unlock()
			l.setInFlight(&pendingOp{kind: "save", key: key, workspace: id, files: snapshot})
			var sv *client.Save
			if sv, err = c.Save(client.WithIdempotencyKey(ctx, key), id, client.QuickSave); err == nil {
				l.mu.Lock()
				l.saves[sv.ID] = savedState{workspace: id, files: snapshot}
				l.inFlight = nil
				l.mu.Unlock()
			}
		case op < 80 && len(live) > 0:
			id := live[rng.IntN(len(live))]
			l.setInFlight(&pendingOp{kind: "resume", save: id, workspace: l.saves[id].workspace})
			if _, err = c.Resume(ctx, id); err == nil {
				l.mu.Lock()
				l.files[l.saves[id].workspace] = copyFiles(l.saves[id].files)
				l.inFlight = nil
				l.mu.Unlock()
			}
		case op < 88 && len(live) > 0:
			id := live[rng.IntN(len(live))]
			key, count := fmt.Sprintf("copy-%d", n), 1+rng.IntN(2)
			l.setInFlight(&pendingOp{kind: "copy", key: key, save: id, count: count})
			var copies []client.Workspace
			if copies, err = c.Copy(client.WithIdempotencyKey(ctx, key), id, count); err == nil {
				l.mu.Lock()
				for _, w := range copies {
					l.files[w.ID] = copyFiles(l.saves[id].files)
				}
				l.inFlight = nil
				l.mu.Unlock()
			}
		case op < 92 && pausable:
			// pausing changes nothing a caller can see (files, saves); the next
			// write wakes the workspace
			_, err = c.PauseWorkspace(ctx, ws[rng.IntN(len(ws))])
		case op < 95 && len(live) > 0:
			id := live[rng.IntN(len(live))]
			l.setInFlight(&pendingOp{kind: "delete", save: id})
			if err = c.DeleteSave(ctx, id); err == nil {
				l.mu.Lock()
				l.deleted[id] = true
				l.inFlight = nil
				l.mu.Unlock()
			}
		case len(dead) > 0:
			id := dead[rng.IntN(len(dead))]
			l.setInFlight(&pendingOp{kind: "undelete", save: id})
			if _, err = c.UndeleteSave(ctx, id); err == nil {
				l.mu.Lock()
				delete(l.deleted, id)
				l.inFlight = nil
				l.mu.Unlock()
			}
		}
		if err != nil {
			return // the engine was killed (or the round ended)
		}
	}
}

func (l *ledger) setInFlight(op *pendingOp) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight = op
}

// settleInFlight decides whether the operation cut off by the kill took effect,
// accepts only "it did" or "it did not", and updates the ledger to match.
func settleInFlight(e *Env, c *client.Client, l *ledger, fail func(string, ...any)) {
	op := l.inFlight
	l.inFlight = nil
	if op == nil {
		return
	}
	switch op.kind {
	// Keyed operations: retry with the same key. The engine must return the
	// original result if the first attempt took effect, or do it now if not,
	// and never both (the exact counts below catch a duplicate).
	case "create":
		w, err := c.CreateWorkspace(client.WithIdempotencyKey(e.Ctx, op.key), client.CreateWorkspace{})
		if err != nil {
			fail("retry of interrupted create %s: %v", op.key, err)
		}
		if l.files[w.ID] == nil {
			l.files[w.ID] = map[string]string{}
		}
	case "save":
		sv, err := c.Save(client.WithIdempotencyKey(e.Ctx, op.key), op.workspace, client.QuickSave)
		if err != nil {
			fail("retry of interrupted save %s: %v", op.key, err)
		}
		l.saves[sv.ID] = savedState{workspace: op.workspace, files: op.files}
	case "copy":
		copies, err := c.Copy(client.WithIdempotencyKey(e.Ctx, op.key), op.save, op.count)
		if err != nil {
			fail("retry of interrupted copy %s: %v", op.key, err)
		}
		for _, w := range copies {
			l.files[w.ID] = copyFiles(l.saves[op.save].files)
		}
	case "resume":
		before, after := l.files[op.workspace], l.saves[op.save].files
		got := map[string]string{}
		for path := range union(before, after) {
			if data, err := c.ReadFile(e.Ctx, op.workspace, path); err == nil {
				got[path] = string(data)
			}
		}
		switch {
		case sameFiles(got, before):
		case sameFiles(got, after):
			l.files[op.workspace] = copyFiles(after) // the resume completed
		default:
			fail("workspace %s is neither before nor after the interrupted resume of %s: %v", op.workspace, op.save, got)
		}
	case "delete", "undelete":
		sv, err := c.GetSave(e.Ctx, op.save)
		if err != nil {
			fail("save %s after interrupted %s: %v", op.save, op.kind, err)
		}
		switch sv.State {
		case "deleted":
			l.deleted[op.save] = true
		case "ready":
			delete(l.deleted, op.save)
		default:
			fail("save %s is %s after interrupted %s", op.save, sv.State, op.kind)
		}
	}
}

func union(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func sameFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// verifyAfterRestart checks every acknowledged fact and that nothing is half done.
func verifyAfterRestart(e *Env, c *client.Client, data string, l *ledger, round int) {
	e.T.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	fail := func(format string, args ...any) {
		e.T.Fatalf("after kill %d: "+format, append([]any{round}, args...)...)
	}
	settleInFlight(e, c, l, fail)

	listed, err := c.ListWorkspaces(e.Ctx)
	if err != nil {
		fail("list workspaces: %v", err)
	}
	byID := map[string]client.Workspace{}
	for _, w := range listed {
		byID[w.ID] = w
		if w.State != "running" && w.State != "stopped" && w.State != "paused" {
			fail("workspace %s left %s", w.ID, w.State)
		}
	}
	if len(listed) != len(l.files) {
		fail("%d workspaces listed, %d acknowledged: a retry duplicated one or one was lost", len(listed), len(l.files))
	}
	for id, files := range l.files {
		if _, ok := byID[id]; !ok {
			fail("acknowledged workspace %s is gone", id)
		}
		listedSaves, err := c.ListSaves(e.Ctx, id)
		if err != nil {
			fail("list saves of %s: %v", id, err)
		}
		want, got := 0, 0
		for sid, st := range l.saves {
			if st.workspace == id && !l.deleted[sid] {
				want++
			}
		}
		for _, sv := range listedSaves {
			if !sv.Auto { // a paused workspace's own save is the engine's
				got++
			}
		}
		if got != want {
			fail("workspace %s has %d saves, %d acknowledged", id, got, want)
		}
		for path, want := range files {
			got, err := c.ReadFile(e.Ctx, id, path)
			if err != nil || string(got) != want {
				fail("workspace %s file %s: got %q, %v; want %q", id, path, got, err, want)
			}
		}
	}

	for id, st := range l.saves {
		sv, err := c.GetSave(e.Ctx, id)
		if err != nil {
			fail("acknowledged save %s: %v", id, err)
		}
		want := "ready"
		if l.deleted[id] {
			want = "deleted"
		}
		if sv.State != want {
			fail("save %s is %s, want %s", id, sv.State, want)
		}
		_ = st
	}

	// no half-written saves or half-swapped filesystems left on disk
	entries, _ := os.ReadDir(filepath.Join(data, "saves"))
	for _, d := range entries {
		if strings.HasPrefix(d.Name(), ".tmp-") {
			fail("half-written save left: %s", d.Name())
		}
	}
	wsDirs, _ := os.ReadDir(filepath.Join(data, "workspaces"))
	for _, d := range wsDirs {
		for _, leftover := range []string{"rootfs.next", "rootfs.prev"} {
			if _, err := os.Stat(filepath.Join(data, "workspaces", d.Name(), leftover)); err == nil {
				fail("%s left in %s", leftover, d.Name())
			}
		}
	}

	// every live acknowledged save restores exactly what was saved (via a copy,
	// so the ledger's workspaces are not disturbed)
	ids := make([]string, 0, len(l.saves))
	for id := range l.saves {
		if !l.deleted[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		cp, err := c.Copy(e.Ctx, id, 1)
		if err != nil {
			fail("copy of acknowledged save %s: %v", id, err)
		}
		for path, want := range l.saves[id].files {
			got, err := c.ReadFile(e.Ctx, cp[0].ID, path)
			if err != nil || string(got) != want {
				fail("save %s file %s: got %q, %v; want %q", id, path, got, err, want)
			}
		}
		if err := c.DeleteWorkspace(e.Ctx, cp[0].ID); err != nil {
			fail("cleanup: %v", err)
		}
	}
}

var (
	cairndOnce sync.Once
	cairndBin  string
	cairndErr  error
)

// buildCairnd compiles cairnd once per test run.
func buildCairnd(e *Env) string {
	e.T.Helper()
	cairndOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cairnd-test-")
		if err != nil {
			cairndErr = err
			return
		}
		cairndBin = filepath.Join(dir, "cairnd")
		out, err := exec.Command("go", "build", "-o", cairndBin, "github.com/AQLD/cairn/cmd/cairnd").CombinedOutput()
		if err != nil {
			cairndErr = fmt.Errorf("build cairnd: %v\n%s", err, out)
		}
	})
	if cairndErr != nil {
		e.T.Fatal(cairndErr)
	}
	return cairndBin
}

// startCairnd runs cairnd on data and waits until it answers.
// cairndRuntimeArgs runs cairnd on the same runtime as the rest of the suite.
func cairndRuntimeArgs(e *Env) []string {
	if os.Getenv("CAIRN_TEST_RUNTIME") != "runsc" {
		return []string{"--runtime", "host"}
	}
	args := []string{"--runtime", "runsc"}
	if bin := os.Getenv("CAIRN_RUNSC"); bin != "" {
		args = append(args, "--runsc", bin)
	}
	if tmpl := os.Getenv("CAIRN_TEST_TEMPLATE"); tmpl != "" {
		args = append(args, "--import-template", "base="+tmpl, "--default-template", "base")
	}
	return args
}

func startCairnd(e *Env, bin, data string) (*exec.Cmd, *client.Client) {
	e.T.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	e.Must(err)
	addr := ln.Addr().String()
	ln.Close()

	cmd := exec.Command(bin, append(cairndRuntimeArgs(e), "--data", data, "--listen", addr, "--pause-on-shutdown=false")...)
	cmd.Stdout, cmd.Stderr = nil, nil
	e.Must(cmd.Start())
	c := client.New("http://"+addr, "")
	deadline := time.Now().Add(60 * time.Second) // recovery restarts every sandbox
	for c.Health(e.Ctx) != nil {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			e.T.Fatalf("cairnd did not come up on %s", addr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cmd, c
}

func keys(m map[string]map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
