//go:build linux

// Command cairn-init is process 1 inside every gVisor workspace.
//
// gVisor restores, from a checkpoint, only processes descended from the
// sandbox's init; anything started by `runsc exec` is killed on restore (see
// spike/experiments/exec-restore-variants.sh). So commands are never started by
// `runsc exec` directly. The engine runs `cairn-init exec -- COMMAND`, which asks
// this process to start COMMAND as its own child, receives the command's pipes
// (sandbox-internal, so the command never holds a host file descriptor), relays
// them to its own stdio and exits with the command's status.
//
//	cairn-init                  run as process 1: start commands, reap orphans
//	cairn-init exec -- CMD...   run CMD as a child of process 1
//
// The two talk over an abstract unix socket, which exists only inside the
// sandbox and adds nothing to the workspace's filesystem.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const socketName = "@cairn-init"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		args := os.Args[2:]
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		os.Exit(client(args))
	}
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, "cairn-init:", err)
		os.Exit(1)
	}
}

// request is what the exec client sends: the command and the environment and
// working folder `runsc exec` gave the client.
type request struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
}

// reply answers a request: the command's pid, or why it did not start (with the
// exit code a shell would use). It travels with the command's pipes.
type reply struct {
	PID   int    `json:"pid,omitempty"`
	Error string `json:"error,omitempty"`
	Code  int    `json:"code,omitempty"`
}

// exit reports how the command ended, as a shell exit code, with a note when
// the sandbox itself intervened (the memory limit).
type exit struct {
	Code int    `json:"exit"`
	Note string `json:"note,omitempty"`
}

// --- process 1 -----------------------------------------------------------------

// reaper collects every exited child (process 1 inherits all orphans) and hands
// the statuses of the commands it started to whoever waits for them.
type reaper struct {
	mu      sync.Mutex
	waiters map[int]chan syscall.WaitStatus
}

func (r *reaper) loop(sigchld <-chan os.Signal) {
	for range sigchld {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			r.mu.Lock()
			ch := r.waiters[pid]
			delete(r.waiters, pid)
			r.mu.Unlock()
			if ch != nil {
				ch <- ws
			}
		}
	}
}

// start runs a command and registers for its exit. The lock is held across the
// fork so the reaper cannot collect the child before it is registered.
func (r *reaper) start(req request, files []uintptr) (int, chan syscall.WaitStatus, error) {
	argv0, err := lookPath(req.Argv[0], req.Env, req.Dir)
	if err != nil {
		return 0, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pid, err := syscall.ForkExec(argv0, req.Argv, &syscall.ProcAttr{
		Dir:   req.Dir,
		Env:   req.Env,
		Files: files,
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		return 0, nil, err
	}
	ch := make(chan syscall.WaitStatus, 1)
	r.waiters[pid] = ch
	return pid, ch, nil
}

func serve() error {
	sigchld := make(chan os.Signal, 128)
	signal.Notify(sigchld, syscall.SIGCHLD)
	r := &reaper{waiters: map[int]chan syscall.WaitStatus{}}
	go r.loop(sigchld)

	// Stopping the sandbox: pass it on, give everything a moment, leave.
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-term
		_ = syscall.Kill(-1, syscall.SIGTERM)
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}()

	if limit, _ := strconv.ParseInt(os.Getenv("CAIRN_MEMORY_LIMIT"), 10, 64); limit > 0 {
		go watchMemory(limit)
	}

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketName, Net: "unix"})
	if err != nil {
		return err
	}
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			// a restore can interrupt accept; keep serving
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go handle(r, conn)
	}
}

func handle(r *reaper, conn *net.UnixConn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req request
	if json.Unmarshal(line, &req) != nil || len(req.Argv) == 0 {
		_ = send(conn, reply{Error: "bad request", Code: 2}, nil)
		return
	}
	if req.Dir == "" {
		req.Dir = "/"
	}

	// Pipes live inside the sandbox: the command's ends go to the command, ours
	// go to the client.
	var ours, theirs []*os.File
	defer func() {
		for _, f := range append(ours, theirs...) {
			f.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		pr, pw, err := os.Pipe()
		if err != nil {
			_ = send(conn, reply{Error: err.Error(), Code: 126}, nil)
			return
		}
		if i == 0 { // stdin: the command reads, the client writes
			theirs, ours = append(theirs, pr), append(ours, pw)
		} else {
			theirs, ours = append(theirs, pw), append(ours, pr)
		}
	}
	pid, done, err := r.start(req, []uintptr{theirs[0].Fd(), theirs[1].Fd(), theirs[2].Fd()})
	if err != nil {
		code := 126
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, errNotFound) {
			code = 127
		}
		_ = send(conn, reply{Error: err.Error(), Code: code}, nil)
		return
	}
	for _, f := range theirs {
		f.Close()
	}
	theirs = nil
	if err := send(conn, reply{PID: pid}, ours); err != nil {
		killCommand(pid)
		<-done
		return
	}
	for _, f := range ours {
		f.Close()
	}
	ours = nil

	// The client going away (killed for a timeout, or by a restore) ends the
	// command, as closing a terminal would.
	kills := oomKills.since()
	gone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		close(gone)
	}()
	var ws syscall.WaitStatus
	select {
	case ws = <-done:
	case <-gone:
		killCommand(pid)
		<-done
		return
	}
	b, _ := json.Marshal(exit{Code: exitCode(ws), Note: oomKills.notesSince(kills)})
	_, _ = conn.Write(append(b, '\n'))
}

// killCommand stops a command whose client is gone: the command itself is
// killed; the rest of its process group gets a hangup, which `nohup` (and any
// daemon) ignores, so deliberately backgrounded processes keep running.
func killCommand(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

func send(conn *net.UnixConn, rep reply, files []*os.File) error {
	b, _ := json.Marshal(rep)
	var oob []byte
	if len(files) > 0 {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		oob = syscall.UnixRights(fds...)
	}
	_, _, err := conn.WriteMsgUnix(append(b, '\n'), oob, nil)
	return err
}

var errNotFound = errors.New("command not found")

// lookPath finds a command the way a shell would, using the command's own PATH.
func lookPath(name string, env []string, dir string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	path := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, d := range filepath.SplitList(path) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, name)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", errNotFound
}

// --- memory limit ---------------------------------------------------------------

// watchMemory enforces the workspace's memory limit inside the sandbox. gVisor
// shows the limit to the workspace but does not enforce it: past it, the host
// kills the whole sandbox. So, like the kernel's OOM killer, this kills the
// biggest process once the workspace uses more than its limit; the host's own
// limit sits above this one as a backstop.
func watchMemory(limit int64) {
	for {
		time.Sleep(50 * time.Millisecond)
		used, ok := memoryUsed()
		if !ok || used <= limit {
			continue
		}
		// Never kill a process that cannot be the problem; memory not held by
		// processes is the host limit's business.
		if pid, rss := biggestProcess(); pid > 0 && rss >= 4<<20 {
			comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
			_ = syscall.Kill(pid, syscall.SIGKILL)
			note := fmt.Sprintf("out of memory: killed process %d (%s, %d MiB); the workspace's limit is %d MiB",
				pid, strings.TrimSpace(string(comm)), rss>>20, limit>>20)
			fmt.Fprintln(os.Stderr, "cairn-init:", note)
			oomKills.record(note)
			time.Sleep(200 * time.Millisecond) // let the memory go before measuring again
		}
	}
}

// oomKills remembers memory-limit kills so a command that was running at the
// time can be told why something died.
var oomKills killLog

type killLog struct {
	mu    sync.Mutex
	seq   int
	notes []string
}

func (k *killLog) record(note string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seq++
	k.notes = append(k.notes, note)
	if len(k.notes) > 16 {
		k.notes = k.notes[1:]
	}
}

// since returns the position to pass to notesSince later.
func (k *killLog) since() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.seq
}

func (k *killLog) notesSince(pos int) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := min(k.seq-pos, len(k.notes))
	return strings.Join(k.notes[len(k.notes)-n:], "; ")
}

// memoryUsed is what the workspace's processes hold, from the sandbox's own
// accounting: MemTotal - MemAvailable, less tmpfs contents (/dev/shm), which no
// process owns and killing one would not free. File cache is not counted by
// gVisor at all.
func memoryUsed() (int64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	var total, avail, shmem int64 = -1, -1, 0
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = kb << 10
		case "MemAvailable:":
			avail = kb << 10
		case "Shmem:":
			shmem = kb << 10
		}
	}
	if total < 0 || avail < 0 {
		return 0, false
	}
	return total - avail - shmem, true
}

// biggestProcess finds the process with the most resident memory, never
// process 1.
func biggestProcess() (pid int, rss int64) {
	entries, _ := os.ReadDir("/proc")
	page := int64(os.Getpagesize())
	type proc struct {
		pid int
		rss int64
	}
	var procs []proc
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil || p == 1 {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/statm")
		if err != nil {
			continue
		}
		f := strings.Fields(string(data))
		if len(f) < 2 {
			continue
		}
		pages, _ := strconv.ParseInt(f[1], 10, 64)
		procs = append(procs, proc{p, pages * page})
	}
	if len(procs) == 0 {
		return 0, 0
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].rss > procs[j].rss })
	return procs[0].pid, procs[0].rss
}

// --- exec client ---------------------------------------------------------------

func client(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cairn-init exec -- COMMAND [ARGS...]")
		return 2
	}
	dir, _ := os.Getwd()
	conn, err := dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairn-init: workspace init is not running:", err)
		return 126
	}
	defer conn.Close()
	b, _ := json.Marshal(request{Argv: argv, Env: os.Environ(), Dir: dir})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "cairn-init:", err)
		return 126
	}

	// First message: the reply, with the command's pipes attached.
	buf := make([]byte, 4096)
	oob := make([]byte, syscall.CmsgSpace(3*4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairn-init:", err)
		return 126
	}
	rd := bufio.NewReader(io.MultiReader(strings.NewReader(string(buf[:n])), conn))
	line, err := rd.ReadBytes('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairn-init:", err)
		return 126
	}
	var rep reply
	if err := json.Unmarshal(line, &rep); err != nil {
		fmt.Fprintln(os.Stderr, "cairn-init: bad reply:", err)
		return 126
	}
	if rep.Error != "" {
		if rep.Code == 127 {
			fmt.Fprintf(os.Stderr, "%s: command not found\n", argv[0])
		} else {
			fmt.Fprintf(os.Stderr, "%s: %s\n", argv[0], rep.Error)
		}
		return rep.Code
	}
	pipes, err := receivedFiles(oob[:oobn])
	if err != nil || len(pipes) != 3 {
		fmt.Fprintln(os.Stderr, "cairn-init: did not receive the command's pipes")
		return 126
	}

	go func() {
		_, _ = io.Copy(pipes[0], os.Stdin)
		pipes[0].Close()
	}()
	var copies sync.WaitGroup
	for i, out := range []*os.File{os.Stdout, os.Stderr} {
		copies.Add(1)
		go func(src *os.File, dst *os.File) {
			defer copies.Done()
			_, _ = io.Copy(dst, src)
		}(pipes[i+1], out)
	}

	// Signals to this client (a cancelled request) end the command too: closing
	// the connection tells process 1.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		<-sigs
		conn.Close()
		os.Exit(143)
	}()

	line, err = rd.ReadBytes('\n')
	var ex exit
	if err != nil || json.Unmarshal(line, &ex) != nil {
		fmt.Fprintln(os.Stderr, "cairn-init: lost the command")
		return 126
	}
	if ex.Note != "" {
		defer fmt.Fprintln(os.Stderr, "cairn:", ex.Note)
	}
	// Output written just before exit is still in the pipes. Background
	// processes may hold them open forever, so wait only briefly for the end.
	drained := make(chan struct{})
	go func() { copies.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(100 * time.Millisecond):
	}
	return ex.Code
}

// dial reaches process 1, waiting for it if the sandbox has only just started.
func dial() (*net.UnixConn, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketName, Net: "unix"})
		if err == nil || time.Now().After(deadline) {
			return conn, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func receivedFiles(oob []byte) ([]*os.File, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for _, m := range msgs {
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return nil, err
		}
		for _, fd := range fds {
			files = append(files, os.NewFile(uintptr(fd), "pipe"))
		}
	}
	return files, nil
}
