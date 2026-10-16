package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AQLD/cairn/client"
)

// Scenarios that need a real sandbox (runsc): full saves of running processes,
// isolation, limits and network control. On the host runtime they skip, because
// the catalog declares what each one needs.

// background starts a long-running process inside a workspace that outlives the
// command that started it.
func background(e *Env, ws, script string) {
	e.T.Helper()
	e.Sh(ws, "nohup sh -c '"+script+"' >/dev/null 2>&1 &")
}

// counterState reads the state file a counter process keeps.
type counterState struct {
	PID   int     `json:"pid"`
	Start float64 `json:"start"`
	N     int     `json:"n"`
}

const counterProgram = `python3 -c "
import os, time, json
start = time.time(); n = 0
while True:
    n += 1
    json.dump({\"pid\": os.getpid(), \"start\": start, \"n\": n}, open(\"/tmp/c.tmp\", \"w\")); os.replace(\"/tmp/c.tmp\", \"/tmp/counter.json\")
    time.sleep(0.05)
"`

func readCounter(e *Env, ws string) counterState {
	e.T.Helper()
	var st counterState
	for i := 0; i < 100; i++ {
		if out, err := e.C.ReadFile(e.Ctx, ws, "tmp/counter.json"); err == nil && json.Unmarshal(out, &st) == nil && st.N > 0 {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.T.Fatal("counter never started")
	return st
}

// processList lists the command lines of every process in a workspace (from
// /proc: slim images have no ps).
const processList = `for p in /proc/[0-9]*; do tr '\0' ' ' < $p/cmdline 2>/dev/null; echo; done`

// running reads a counter twice and fails unless it is still advancing: the
// last value written before a save is no proof the process came back.
func running(e *Env, ws string) counterState {
	e.T.Helper()
	first := readCounter(e, ws)
	time.Sleep(500 * time.Millisecond)
	second := readCounter(e, ws)
	if second.N <= first.N {
		e.T.Fatalf("counter stuck at %d: the process is not running", first.N)
	}
	return second
}

func init() {
	// G1.5: a running app comes back running.
	register("G1.5", func(e *Env) {
		ws := e.Workspace("devserver")
		e.Sh(ws.ID, "mkdir -p /srv && echo 'served after resume' > /srv/index.html")
		background(e, ws.ID, "cd /srv && exec python3 -m http.server 8000")
		fetch := `python3 -c "import urllib.request,time
for i in range(50):
    try: print(urllib.request.urlopen('http://127.0.0.1:8000/index.html', timeout=2).read().decode(), end=''); break
    except Exception: time.sleep(0.1)"`
		Equal(e, "before save", e.Sh(ws.ID, fetch), "served after resume\n")
		pid := strings.TrimSpace(e.Sh(ws.ID, "for p in /proc/[0-9]*; do grep -qa 'http.server' $p/cmdline 2>/dev/null && basename $p; done | head -1"))

		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		e.Resume(sv.ID)
		Equal(e, "server answers after resume", e.Sh(ws.ID, fetch), "served after resume\n")
		Equal(e, "same server process", strings.TrimSpace(e.Sh(ws.ID, "for p in /proc/[0-9]*; do grep -qa 'http.server' $p/cmdline 2>/dev/null && basename $p; done | head -1")), pid)
	})

	// G4.2: resume continues the exact step it paused at.
	register("G4.2", func(e *Env) {
		ws := e.Workspace("waiting")
		background(e, ws.ID, counterProgram)
		time.Sleep(300 * time.Millisecond)
		before := readCounter(e, ws.ID)
		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		time.Sleep(1 * time.Second) // the agent waits; nothing runs
		e.Resume(sv.ID)
		after := running(e, ws.ID)
		Equal(e, "same process", after.PID, before.PID)
		Equal(e, "same start", after.Start, before.Start)
		if after.N <= before.N {
			e.T.Fatalf("counter went from %d to %d: the process restarted", before.N, after.N)
		}
		// it did not run while saved: with 50 ms ticks, 1 s of running would add ~20
		if after.N-before.N > 25+int(time.Second/(50*time.Millisecond)) {
			e.T.Fatalf("counter advanced %d ticks across the pause: it kept running while saved", after.N-before.N)
		}
	})

	// G4.1 / G4.3: a full save stops the workspace; it refuses commands until resumed.
	register("G4.1", func(e *Env) {
		ws := e.Workspace("stop")
		_, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "state after full save", got.State, "stopped")
	})
	register("G4.3", func(e *Env) {
		ws := e.Workspace("stopped")
		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		_, err = e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"true"}})
		e.ErrCode(err, "conflict")
		e.Resume(sv.ID)
		e.Sh(ws.ID, "true")
	})

	// G3.1: workspaces cannot see each other's files or processes.
	register("G3.1", func(e *Env) {
		a := e.Workspace("a")
		b := e.Workspace("b")
		e.Sh(a.ID, "echo secret-of-a > /root/secret")
		background(e, a.ID, "exec sleep 1234")
		out, err := e.C.Run(e.Ctx, b.ID, "sh", "-c", "cat /root/secret 2>/dev/null; "+processList)
		e.Must(err)
		if strings.Contains(out, "secret-of-a") || strings.Contains(out, "sleep 1234") {
			e.T.Fatalf("workspace b sees workspace a: %q", out)
		}
	})

	// G8.7: host files and processes are invisible; the kernel is gVisor's.
	register("G8.7", func(e *Env) {
		ws := e.Workspace("probe")
		hostKernel, _ := exec.Command("uname", "-r").Output()
		guest := e.Sh(ws.ID, "uname -r")
		if strings.TrimSpace(guest) == strings.TrimSpace(string(hostKernel)) {
			e.T.Fatalf("workspace reports the host kernel %q: system calls reach the host", guest)
		}
		procs := e.Sh(ws.ID, processList)
		for _, host := range []string{"systemd", "containerd", "dockerd", "cairnd", "go test"} {
			if strings.Contains(procs, host) {
				e.T.Fatalf("workspace sees host process %q:\n%s", host, procs)
			}
		}
	})

	// G3.2.2 / G8.4: a memory limit stops the workspace's own process, not the host
	// or the neighbours.
	register("G3.2.2", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "small", Limits: client.Limits{MemoryMB: 256}})
		e.Must(err)
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{
			Command:   []string{"python3", "-c", "b = bytearray(700 * 1024 * 1024); print('allocated')"},
			TimeoutMS: 60_000,
		})
		e.Must(err)
		if res.ExitCode == 0 && strings.Contains(res.Stdout, "allocated") {
			e.T.Fatal("a 700 MB allocation succeeded in a 256 MB workspace")
		}
		// the caller is told why, not left with a bare exit code
		if !strings.Contains(res.Stderr, "memory") {
			e.T.Fatalf("exit %d with no word about memory: %q", res.ExitCode, res.Stderr)
		}
		e.Sh(ws.ID, "true") // the workspace itself survives
	})
	// G3.2.1: a workspace given half a CPU gets about half the work done of an
	// unlimited one. (Measured as throughput: CPU time seen inside a sandbox
	// includes time the host held it back.)
	register("G3.2.1", func(e *Env) {
		spin := `python3 -c "
import time
n, w0 = 0, time.monotonic()
while time.monotonic() - w0 < 3: n += 1
print(n)"`
		// Errors come back to the test goroutine: Fatal must not run in others.
		work := func(limits client.Limits) (float64, error) {
			ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Limits: limits})
			if err != nil {
				return 0, err
			}
			out, err := e.C.Run(e.Ctx, ws.ID, "sh", "-c", spin)
			if err != nil {
				return 0, err
			}
			return strconv.ParseFloat(strings.TrimSpace(out), 64)
		}
		// side by side, so anything else loading the machine hits both alike
		var full, half float64
		var fullErr, halfErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); full, fullErr = work(client.Limits{}) }()
		go func() { defer wg.Done(); half, halfErr = work(client.Limits{CPUs: 0.5}) }()
		wg.Wait()
		e.Must(fullErr)
		e.Must(halfErr)
		if ratio := half / full; ratio > 0.7 || ratio < 0.2 {
			e.T.Fatalf("a 0.5 CPU workspace did %.2f of an unlimited one's work", ratio)
		}
	})

	// G3.2.4: even a stampede of allocations (several processes at once, faster
	// than any watcher) costs at most the running processes, never the files.
	register("G3.2.4", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "stampede", Limits: client.Limits{MemoryMB: 256}})
		e.Must(err)
		e.Sh(ws.ID, "echo precious > /root/work.txt")
		_, _ = e.C.Exec(e.Ctx, ws.ID, client.Exec{
			Command: []string{"sh", "-c", `for i in 1 2 3 4; do python3 -c "
x = []
while True: x.append(b'x' * (64 << 20))" & done; wait`},
			TimeoutMS: 60_000,
		})
		Equal(e, "workspace answers", e.Sh(ws.ID, "echo ok"), "ok\n")
		Equal(e, "files intact", e.Sh(ws.ID, "cat /root/work.txt"), "precious\n")
	})
	// G3.2.5: memory no process owns (files in /dev/shm) and file reads do not
	// count against processes: nothing is killed for them.
	register("G3.2.5", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "shm", Limits: client.Limits{MemoryMB: 256}})
		e.Must(err)
		background(e, ws.ID, counterProgram)
		before := running(e, ws.ID)
		e.Sh(ws.ID, "dd if=/dev/zero of=/dev/shm/blob bs=1M count=200 status=none")
		e.Sh(ws.ID, "dd if=/dev/zero of=/root/big bs=1M count=300 status=none && cat /root/big > /dev/null")
		Equal(e, "100 MB alongside", e.Sh(ws.ID, `python3 -c "b = bytearray(100 << 20); b[::4096] = b'x' * len(b[::4096]); print('ok')"`), "ok\n")
		Equal(e, "background process untouched", running(e, ws.ID).PID, before.PID)
	})

	register("G8.4", func(e *Env) {
		hog, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "hog", Limits: client.Limits{MemoryMB: 256}})
		e.Must(err)
		neighbour := e.Workspace("neighbour")
		_, _ = e.C.Exec(e.Ctx, hog.ID, client.Exec{Command: []string{"python3", "-c", "x = [bytearray(64 << 20) for _ in range(40)]"}, TimeoutMS: 60_000})
		Equal(e, "neighbour unaffected", e.Sh(neighbour.ID, "echo ok"), "ok\n")
	})

	// G8.3: a fork bomb is contained.
	register("G8.3", func(e *Env) {
		bomb := e.Workspace("bomb")
		neighbour := e.Workspace("neighbour")
		res, err := e.C.Exec(e.Ctx, bomb.ID, client.Exec{
			Command:   []string{"python3", "-c", "import os\nwhile True:\n    try: os.fork()\n    except OSError: pass"},
			TimeoutMS: 5_000,
		})
		e.Must(err)
		_ = res
		start := time.Now()
		Equal(e, "neighbour still answers", e.Sh(neighbour.ID, "echo ok"), "ok\n")
		if d := time.Since(start); d > 5*time.Second {
			e.T.Fatalf("neighbour took %v to answer during the fork bomb", d)
		}
		e.Must(e.C.DeleteWorkspace(e.Ctx, bomb.ID)) // and the bomb can be cleaned up
	})

	// G5.3: network is off unless switched on per workspace.
	register("G5.3", func(e *Env) {
		probe := probeInternet
		off := e.Workspace("offline")
		Equal(e, "default workspace", e.Sh(off.ID, probe), "unreachable\n")
		on, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "online", Network: true})
		e.Must(err)
		Equal(e, "network switched on", e.Sh(on.ID, probe), "reachable\n")
	})

	// G8.6: cloud metadata is unreachable even with network on, including
	// for a workspace adopted by a restarted engine.
	register("G8.6", func(e *Env) {
		on, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "online", Network: true})
		e.Must(err)
		checkMetadataBlocked(e, on.ID)
		if e.Engine != nil {
			e.Restart() // the sandbox keeps running and is adopted
			checkMetadataBlocked(e, on.ID)
			Equal(e, "network after the restart", e.Sh(on.ID, probeInternet), "reachable\n")
		}
	})
}

const probeInternet = `python3 -c "
import socket
try:
    socket.create_connection(('1.1.1.1', 443), timeout=3); print('reachable')
except OSError:
    print('unreachable')"`

// checkMetadataBlocked probes the cloud metadata address from the workspace.
// Where nothing answers there (a dev VM) the probe passes either way, so as
// root it also checks the firewall itself: the block must come before the
// rule letting the workspace's traffic through.
func checkMetadataBlocked(e *Env, ws string) {
	e.T.Helper()
	out := e.Sh(ws, `python3 -c "
import socket
try:
    socket.create_connection(('169.254.169.254', 80), timeout=3); print('reachable')
except OSError:
    print('blocked')"`)
	Equal(e, "cloud metadata", out, "blocked\n")
	if os.Geteuid() != 0 || e.Engine == nil {
		return
	}
	rules, err := exec.Command("iptables", "-S", "FORWARD").Output()
	e.Must(err)
	drop, accept := -1, -1
	for i, line := range strings.Split(string(rules), "\n") {
		if drop < 0 && strings.Contains(line, "-d 169.254.0.0/16") && strings.HasSuffix(line, "-j DROP") {
			drop = i
		}
		if accept < 0 && strings.Contains(line, "-i ch") && strings.HasSuffix(line, "-j ACCEPT") {
			accept = i
		}
	}
	if drop < 0 || accept < 0 || drop > accept {
		e.T.Fatalf("metadata block must precede the workspace's accept rule:\n%s", rules)
	}
}
