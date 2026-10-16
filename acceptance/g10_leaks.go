package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G10.7: every workspace operation cleans up after itself. After cycles of
// create, run, save, resume, pause, copy and delete, the host has exactly what
// it had before: no mounts, loop devices, network namespaces, firewall rules,
// cgroups or sandboxes left over, and the engine holds no extra files or
// goroutines.

const leakCycles = 4

func init() {
	register("G10.7", func(e *Env) {
		cycle := func(i int) {
			ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: fmt.Sprintf("cycle-%d", i), Network: i%2 == 0})
			e.Must(err)
			e.Write(ws.ID, "work.txt", "data")
			Equal(e, "command", e.Sh(ws.ID, "cat work.txt"), "data")
			if e.Info.Capabilities.PersistentProcesses {
				background(e, ws.ID, "sleep 600")
			}
			quick := e.Save(ws.ID)
			if e.Info.Capabilities.FullSave {
				full, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
				e.Must(err)
				e.Resume(full.ID)
				_, err = e.C.PauseWorkspace(e.Ctx, ws.ID)
				e.Must(err)
				Equal(e, "wakes", e.Sh(ws.ID, "echo up"), "up\n")
			}
			copies, err := e.C.Copy(e.Ctx, quick.ID, 1)
			e.Must(err)
			e.Must(e.C.DeleteWorkspace(e.Ctx, copies[0].ID))
			e.Must(e.C.DeleteWorkspace(e.Ctx, ws.ID))
		}

		cycle(0) // warm up: the first network workspace sets up what all of them share
		before := hostResources(e)
		// while workspaces exist the counts must move, or this test sees nothing
		probe, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "probe", Network: true})
		e.Must(err)
		e.T.Logf("one workspace: %v", hostResources(e))
		e.Must(e.C.DeleteWorkspace(e.Ctx, probe.ID))
		for i := 1; i <= leakCycles; i++ {
			cycle(i)
		}
		// goroutines and sandboxes wind down asynchronously: give them a moment
		var after map[string]int
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			after = hostResources(e)
			if leaked(before, after) == "" || time.Now().After(deadline) {
				break
			}
		}
		e.T.Logf("before: %v", before)
		e.T.Logf("after:  %v", after)
		if l := leaked(before, after); l != "" {
			e.T.Fatalf("after %d create-to-delete cycles: %s", leakCycles, l)
		}
	})
}

// leaked describes what grew between two counts ("" if nothing did). The
// engine's goroutines may vary by a few (idle HTTP connections, timers).
func leaked(before, after map[string]int) string {
	var out []string
	for k, a := range after {
		slack := 0
		if k == "engine goroutines" {
			slack = 10
		}
		if b := before[k]; a > b+slack {
			out = append(out, fmt.Sprintf("%s %d -> %d", k, b, a))
		}
	}
	return strings.Join(out, ", ")
}

// hostResources counts what the engine creates on the host. Parts that need
// root or Linux are counted only where they can be.
func hostResources(e *Env) map[string]int {
	e.T.Helper()
	n := map[string]int{"engine goroutines": goruntime.NumGoroutine()}
	if fds, err := os.ReadDir("/proc/self/fd"); err == nil {
		n["engine files"] = len(fds)
	}
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) > 4 && strings.HasPrefix(f[4], e.DataDir+"/") {
				n["mounts"]++
			}
		}
	}
	backing, _ := filepath.Glob("/sys/block/loop*/loop/backing_file")
	for _, p := range backing {
		if b, err := os.ReadFile(p); err == nil && strings.HasPrefix(string(b), e.DataDir+"/") {
			n["loop devices"]++
		}
	}
	if ns, err := os.ReadDir("/var/run/netns"); err == nil {
		for _, d := range ns {
			if strings.HasPrefix(d.Name(), "cairn-") {
				n["network namespaces"]++
			}
		}
	}
	if cg, err := os.ReadDir("/sys/fs/cgroup/cairn"); err == nil {
		for _, d := range cg {
			if d.IsDir() {
				n["cgroups"]++
			}
		}
	}
	if os.Geteuid() == 0 {
		for _, table := range []string{"filter", "nat"} {
			if out, err := exec.Command("iptables-save", "-t", table).Output(); err == nil {
				for _, line := range strings.Split(string(out), "\n") {
					if strings.Contains(line, " ch") && strings.HasPrefix(line, "-A") {
						n["firewall rules"]++
					}
				}
			}
		}
	}
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	root := "--root=" + filepath.Join(e.DataDir, "runsc")
	for _, p := range procs {
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), root) {
			n["runsc processes"]++
		}
	}
	return n
}
