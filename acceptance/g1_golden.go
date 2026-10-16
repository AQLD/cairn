package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G1.7: golden workloads, the E0 spike's real programs, survive a full save
// and resume through the engine: the same process carries on, with its open
// files, servers, watchers, downloads and browser pages.
//
// Each workload is a template built from spike/workloads/<name> (its image,
// exported: `docker build -t w spike/workloads/<name> && docker export $(docker
// create w) > <name>.tar`). CAIRN_TEST_GOLDEN names the folder holding the
// tars; devenv/accept-runsc.sh builds them.

type goldenWorkload struct {
	name    string
	network bool
	warm    time.Duration // how long it may take to become ready
	settle  time.Duration // let it run a little before saving
	// ready overrides the probe's own "ready" (when to save)
	ready func(state) bool
	// midway, if set, must hold for the saved workspace (stopped by the save,
	// so read through its files), or the attempt proves nothing (the work
	// finished first) and is tried again
	midway func(e *Env, ws string) bool
	check  func(e *Env, ws string, before, after state)
}

type state map[string]any

func (s state) num(k string) float64 {
	v, _ := s[k].(float64)
	return v
}

func init() {
	register("G1.7.1", func(e *Env) {
		runGolden(e, goldenWorkload{name: "python-open-files", warm: time.Minute, settle: 2 * time.Second,
			check: func(e *Env, ws string, b, a state) {
				sameProcess(e, b, a, "pid", "started")
				rising(e, b, a, "counter")
				Equal(e, "append offset equals the file's size", a.num("log_offset"), a.num("log_size"))
				Equal(e, "lines the reader skipped or repeated", a.num("read_gaps"), 0)
			}})
	})
	register("G1.7.2", func(e *Env) {
		runGolden(e, goldenWorkload{name: "node-dev-server", warm: time.Minute, settle: 2 * time.Second,
			check: func(e *Env, ws string, b, a state) {
				sameProcess(e, b, a, "pid", "started")
				rising(e, b, a, "hits")
				// the restored watcher still notices edits
				e.Sh(ws, `echo "export const v = 2" > /app/src/index.js`)
				deadline := time.Now().Add(10 * time.Second)
				for probeGolden(e, ws).num("reloads") <= a.num("reloads") {
					if time.Now().After(deadline) {
						e.T.Fatal("the file watcher did not fire after the resume")
					}
					time.Sleep(200 * time.Millisecond)
				}
			}})
	})
	register("G1.7.3", func(e *Env) {
		inFlight := func(s state) bool { _, done := s["exit_code"].(float64); return !done }
		runGolden(e, goldenWorkload{name: "package-install", network: true, warm: 3 * time.Minute,
			// save as early as possible: on a fast network the whole download takes seconds
			ready: func(s state) bool { return inFlight(s) && s.num("bytes") > 2e6 },
			midway: func(e *Env, ws string) bool {
				_, err := e.C.ReadFile(e.Ctx, ws, "spike/exit_code") // written when pip ends
				return err != nil
			},
			check: func(e *Env, ws string, b, a state) {
				sameProcess(e, b, a, "started", "installer_pid")
				// the download was cut mid-transfer; pip must retry and finish
				deadline := time.Now().Add(10 * time.Minute)
				for {
					s := probeGolden(e, ws)
					if code, done := s["exit_code"].(float64); done {
						if code != 0 {
							log, _ := e.C.ReadFile(e.Ctx, ws, "spike/pip.log")
							e.T.Fatalf("the install failed after the resume: %v\n%s", s, tail(string(log), 1500))
						}
						return
					}
					if time.Now().After(deadline) {
						e.T.Fatalf("the install is still running 10 minutes after the resume: %v", s)
					}
					time.Sleep(5 * time.Second)
				}
			}})
	})
	register("G1.7.4", func(e *Env) {
		runGolden(e, goldenWorkload{name: "headless-browser", warm: 90 * time.Second, settle: 3 * time.Second,
			check: func(e *Env, ws string, b, a state) {
				sameProcess(e, b, a, "browser_pid", "page_started")
				rising(e, b, a, "count")
			}})
	})
}

func runGolden(e *Env, w goldenWorkload) {
	e.T.Helper()
	dir := os.Getenv("CAIRN_TEST_GOLDEN")
	if dir == "" {
		e.T.Skip("needs CAIRN_TEST_GOLDEN: a folder of templates built from spike/workloads (devenv/accept-runsc.sh builds them)")
	}
	f, err := os.Open(filepath.Join(dir, w.name+".tar"))
	if err != nil {
		e.T.Skipf("no template for %s: %v", w.name, err)
	}
	_, err = e.C.ImportTemplate(e.Ctx, "golden-"+w.name, f)
	f.Close()
	e.Must(err)
	if w.ready == nil {
		w.ready = func(s state) bool { return s["ready"] == true }
	}

	for attempt := 1; ; attempt++ {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: w.name, Template: "golden-" + w.name, Network: w.network})
		e.Must(err)
		background(e, ws.ID, "exec /spike/start.sh")
		deadline := time.Now().Add(w.warm)
		for !w.ready(probeGolden(e, ws.ID)) {
			if time.Now().After(deadline) {
				e.T.Fatalf("%s not ready after %s: %v", w.name, w.warm, probeGolden(e, ws.ID))
			}
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(w.settle)

		before := probeGolden(e, ws.ID)
		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		if w.midway != nil && !w.midway(e, ws.ID) {
			e.T.Logf("attempt %d: %s finished before the save; trying again", attempt, w.name)
			e.Must(e.C.DeleteWorkspace(e.Ctx, ws.ID))
			if attempt == 3 {
				e.T.Fatalf("%s always finished before the save could interrupt it", w.name)
			}
			continue
		}
		e.Resume(sv.ID)
		time.Sleep(2 * time.Second) // let it make progress
		after := probeGolden(e, ws.ID)
		e.T.Logf("before: %v", before)
		e.T.Logf("after:  %v", after)
		w.check(e, ws.ID, before, after)
		return
	}
}

// probeGolden runs the workload's probe, which prints its state as JSON.
func probeGolden(e *Env, ws string) state {
	e.T.Helper()
	res, err := e.C.Exec(e.Ctx, ws, client.Exec{Command: []string{"/spike/probe"}, TimeoutMS: 30_000})
	e.Must(err)
	var s state
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &s); err != nil {
		return state{"probe_error": err.Error(), "stdout": res.Stdout, "stderr": res.Stderr}
	}
	return s
}

// sameProcess fails unless the keys (pid, start time) are unchanged: the
// workload carried on rather than restarting.
func sameProcess(e *Env, b, a state, keys ...string) {
	e.T.Helper()
	for _, k := range keys {
		if b.num(k) == 0 || a.num(k) != b.num(k) {
			e.T.Fatalf("%s changed from %v to %v: the workload restarted instead of resuming", k, b[k], a[k])
		}
	}
}

// rising fails unless the counter k advanced after the resume.
func rising(e *Env, b, a state, k string) {
	e.T.Helper()
	if a.num(k) <= b.num(k) {
		e.T.Fatalf("%s did not advance after the resume (%v -> %v)", k, b[k], a[k])
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
