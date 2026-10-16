package runtime

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Runsc runs each workspace in its own gVisor sandbox, driving runsc directly
// (not through Docker) so Cairn controls exactly how saves and restores happen.
//
// The sandbox runs on the engine's workspace folder with --overlay2=none, so
// the folder on the host stays the source of truth: the file API, quick saves
// and copies all read it directly. A full save is `runsc checkpoint` (memory
// and processes) plus the engine's archive of that folder, taken after the
// checkpoint has stopped the sandbox. A full save restores even after the folder
// has been re-extracted at another path (a copy, or another server).
//
// Lessons from the E0 spike, kept here:
//   - runsc moves a network namespace's address and routes into its own network
//     stack, leaving the namespace bare, so networking is re-applied before every
//     start and every restore.
//   - flags must be identical for checkpoint and restore.
//   - gVisor restores only processes descended from the sandbox's init; anything
//     `runsc exec` started is killed on restore. So every workspace runs
//     cairn-init as process 1, and commands are started by it (see cmd/cairn-init).
type Runsc struct {
	Binary   string // path to runsc: the release new sandboxes boot with
	StateDir string // runsc --root: its record of running sandboxes
	Init     string // path to the cairn-init binary for new sandboxes
	Log      *slog.Logger
	// Releases is the folder holding one subfolder per installed gVisor
	// release (<Releases>/<release>/runsc), searched when a full save made
	// with another release is restored. Default: two levels up from Binary's
	// real path, i.e. /opt/gvisor for /opt/gvisor/<release>/runsc.
	Releases string

	versionMu sync.Mutex
	versions  map[string]string // runsc path -> version

	mu sync.Mutex // serializes network setup
}

// NewRunsc returns the gVisor runtime. stateDir must be on local disk and
// survives engine restarts (sandboxes outlive the engine process). init is the
// cairn-init binary (built from cmd/cairn-init for the host's architecture).
func NewRunsc(binary, stateDir, init string) *Runsc {
	return &Runsc{Binary: binary, StateDir: stateDir, Init: init}
}

// initInSandbox is where cairn-init appears inside a workspace: on the sandbox's
// own /dev, so it adds nothing to the workspace's filesystem.
const initInSandbox = "/dev/cairn-init"

// initFile is the workspace's own copy of cairn-init. A running sandbox (and a
// checkpoint of it) depends on exactly this binary, so it is copied per
// workspace and travels inside full saves: an engine upgrade never changes the
// binary under a sandbox or a save.
func initFile(spec Spec) string { return filepath.Join(filepath.Dir(spec.RootDir), "cairn-init") }

const initSaveName = "cairn-init"

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func (r *Runsc) Name() string { return "runsc" }

func (r *Runsc) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// heal restarts a workspace whose sandbox died under it (the host killed it
// for memory, or it crashed). Processes are gone, the filesystem is not: the
// workspace comes back as after a reboot.
func (r *Runsc) heal(ctx context.Context, spec Spec, why string) error {
	r.log().Warn("workspace sandbox is gone; restarting it from its filesystem", "workspace", spec.ID, "reason", why)
	return r.Start(ctx, spec)
}

// Version is the release new sandboxes boot with, recorded in every save.
func (r *Runsc) Version() string { return r.versionOf(r.Binary) }

// versionOf is a runsc binary's release ("release-20261005.0").
func (r *Runsc) versionOf(bin string) string {
	r.versionMu.Lock()
	defer r.versionMu.Unlock()
	if v, ok := r.versions[bin]; ok {
		return v
	}
	v := "unknown"
	if out, err := exec.Command(bin, "--version").Output(); err == nil {
		first := strings.SplitN(string(out), "\n", 2)[0]
		v = strings.TrimSpace(strings.TrimPrefix(first, "runsc version"))
	}
	if r.versions == nil {
		r.versions = map[string]string{}
	}
	r.versions[bin] = v
	return v
}

// --- which runsc runs a sandbox ----------------------------------------------
//
// A checkpoint restores only with the gVisor release that made it, and a
// sandbox must be driven (exec, checkpoint, kill) by the release running it.
// So each workspace records the binary its sandbox runs on; a fresh boot uses
// the current release, a restore the release the checkpoint names. Releases
// are kept side by side (deploy/install-gvisor.sh RELEASE), so upgrading
// gVisor never strands a full save or a paused workspace.

// binaryFile records the real path of the runsc running the workspace's sandbox.
func binaryFile(spec Spec) string { return filepath.Join(filepath.Dir(spec.RootDir), "runsc-binary") }

// versionFile, inside a checkpoint, names the release that made it.
const versionFile = "runsc-version"

// binaryFor is the runsc that drives the workspace's sandbox.
func (r *Runsc) binaryFor(spec Spec) string {
	if b, err := os.ReadFile(binaryFile(spec)); err == nil {
		if p := strings.TrimSpace(string(b)); p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return r.Binary
}

// useBinary records bin (by its real path, so relinking runsc to a newer
// release later does not change it) as the workspace's runsc.
func (r *Runsc) useBinary(spec Spec, bin string) error {
	if p, err := exec.LookPath(bin); err == nil {
		bin = p
	}
	if p, err := filepath.EvalSymlinks(bin); err == nil {
		bin = p
	}
	tmp := binaryFile(spec) + ".tmp"
	if err := os.WriteFile(tmp, []byte(bin+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, binaryFile(spec))
}

// release finds the installed runsc of the given version.
func (r *Runsc) release(version string) (string, error) {
	if version == "" || version == r.Version() {
		return r.Binary, nil
	}
	dir := r.Releases
	if dir == "" {
		bin := r.Binary
		if p, err := exec.LookPath(bin); err == nil {
			bin = p
		}
		if p, err := filepath.EvalSymlinks(bin); err == nil {
			bin = p
		}
		dir = filepath.Dir(filepath.Dir(bin))
	}
	candidates, _ := filepath.Glob(filepath.Join(dir, "*", "runsc"))
	for _, c := range candidates {
		if r.versionOf(c) == version {
			return c, nil
		}
	}
	folder := strings.TrimSuffix(strings.TrimPrefix(version, "release-"), ".0")
	return "", fmt.Errorf("%w: this full save was made with gVisor %s, which is not installed in %s (install it with deploy/install-gvisor.sh %s)",
		ErrUnsupported, version, dir, folder)
}

func (r *Runsc) Capabilities() Capabilities {
	return Capabilities{
		Isolation:           true,
		FullSave:            true,
		ResourceLimits:      true, // CPU, memory and process count via cgroups
		DiskLimits:          false,
		NetworkControl:      true,
		PersistentProcesses: true,
	}
}

// flags are the global runsc flags for a workspace. They must not change between
// a checkpoint and its restore.
func (r *Runsc) flags(spec Spec) []string {
	f := []string{"--root", r.StateDir, "--overlay2=none"}
	if !spec.Network {
		f = append(f, "--network=none")
	}
	return f
}

func (r *Runsc) cmd(ctx context.Context, spec Spec, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, r.binaryFor(spec), append(r.flags(spec), args...)...)
}

// run executes a runsc subcommand and returns its error with runsc's own output.
func (r *Runsc) run(ctx context.Context, spec Spec, args ...string) error {
	out, err := r.cmd(ctx, spec, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("runsc %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// containerID is the sandbox name for a workspace.
func containerID(spec Spec) string { return "cairn-" + spec.ID }

func bundleDir(spec Spec) string { return filepath.Join(filepath.Dir(spec.RootDir), "bundle") }

// status is runsc's view of a sandbox: "running", "stopped", "paused", or ""
// if it does not exist.
func (r *Runsc) status(ctx context.Context, spec Spec) string {
	out, err := r.cmd(ctx, spec, "state", containerID(spec)).Output()
	if err != nil {
		return ""
	}
	var st struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(out, &st) != nil {
		return ""
	}
	return st.Status
}

// Start makes sure the workspace's sandbox is running. It is idempotent: the
// engine calls it again after its own restart, when sandboxes may still be up.
func (r *Runsc) Start(ctx context.Context, spec Spec) error {
	switch r.status(ctx, spec) {
	case "running":
		// adopted after an engine restart: bring its firewall up to what this
		// engine enforces (only the host side: the sandbox owns its interface)
		return r.firewall(spec)
	case "":
	default:
		_ = r.run(ctx, spec, "delete", "--force", containerID(spec))
	}
	// a fresh boot takes the engine's current cairn-init and gVisor
	if err := copyExecutable(r.Init, initFile(spec)); err != nil {
		return fmt.Errorf("install cairn-init: %w", err)
	}
	if err := r.useBinary(spec, r.Binary); err != nil {
		return err
	}
	if err := r.writeBundle(spec); err != nil {
		return err
	}
	if err := r.netUp(spec); err != nil {
		return err
	}
	if err := r.detached(ctx, spec, "run", "--detach", "--bundle", bundleDir(spec), containerID(spec)); err != nil {
		return err
	}
	return nil
}

// detached runs a runsc command that leaves a sandbox behind. Its stdio goes to
// a log file, never to the engine, so the sandbox cannot hold the engine's pipes.
func (r *Runsc) detached(ctx context.Context, spec Spec, args ...string) error {
	logPath := filepath.Join(filepath.Dir(spec.RootDir), "runsc.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	c := r.cmd(ctx, spec, args...)
	c.Stdin, c.Stdout, c.Stderr = nil, logf, logf
	if err := c.Run(); err != nil {
		tail, _ := os.ReadFile(logPath)
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("runsc %s: %v: %s", args[0], err, strings.TrimSpace(string(tail)))
	}
	return nil
}

func (r *Runsc) Stop(ctx context.Context, spec Spec) error {
	if r.status(ctx, spec) == "" {
		r.netDown(spec)
		return nil
	}
	_ = r.run(ctx, spec, "kill", containerID(spec), "KILL")
	if err := r.run(ctx, spec, "delete", "--force", containerID(spec)); err != nil && r.status(ctx, spec) != "" {
		return err
	}
	r.netDown(spec)
	return nil
}

// Checkpoint writes memory and processes into dir; the sandbox stops.
func (r *Runsc) Checkpoint(ctx context.Context, spec Spec, dir string) error {
	if err := r.run(ctx, spec, "checkpoint", "--image-path", dir, containerID(spec)); err != nil {
		return err
	}
	_ = r.run(ctx, spec, "delete", "--force", containerID(spec))
	if err := os.WriteFile(filepath.Join(dir, versionFile), []byte(r.versionOf(r.binaryFor(spec))+"\n"), 0o644); err != nil {
		return err
	}
	// the processes in the checkpoint were started by this exact binary
	return copyExecutable(initFile(spec), filepath.Join(dir, initSaveName))
}

// Restore starts the workspace from a checkpoint. The workspace folder must
// already hold the filesystem saved with it (the engine extracts it first).
func (r *Runsc) Restore(ctx context.Context, spec Spec, dir string) error {
	var version string // checkpoints from before versions were recorded: the current release
	if b, err := os.ReadFile(filepath.Join(dir, versionFile)); err == nil {
		version = strings.TrimSpace(string(b))
	}
	bin, err := r.release(version)
	if err != nil {
		return err
	}
	if r.status(ctx, spec) != "" {
		_ = r.run(ctx, spec, "delete", "--force", containerID(spec))
	}
	if err := r.useBinary(spec, bin); err != nil {
		return err
	}
	if err := copyExecutable(filepath.Join(dir, initSaveName), initFile(spec)); err != nil {
		return fmt.Errorf("checkpoint has no usable cairn-init: %w", err)
	}
	if err := r.writeBundle(spec); err != nil {
		return err
	}
	if err := r.netUp(spec); err != nil {
		return err
	}
	if err := r.detached(ctx, spec, "restore", "--detach", "--image-path", dir, "--bundle", bundleDir(spec), containerID(spec)); err != nil {
		return err
	}
	return nil
}

// Exec runs one command in the sandbox. A command that times out (or whose
// request is cancelled) is killed inside the sandbox, not just its runsc client.
func (r *Runsc) Exec(ctx context.Context, spec Spec, req ExecRequest) (ExecResult, error) {
	if len(req.Argv) == 0 {
		return ExecResult{}, errors.New("empty command")
	}
	cwd := "/"
	if req.Dir != "" {
		clean := path.Clean("/" + filepath.ToSlash(req.Dir))
		if strings.HasPrefix(path.Clean(req.Dir), "..") {
			return ExecResult{}, fmt.Errorf("working directory %q is outside the workspace", req.Dir)
		}
		cwd = clean
	}
	if st := r.status(ctx, spec); st != "running" {
		if err := r.heal(ctx, spec, "sandbox "+cmp.Or(st, "missing")); err != nil {
			return ExecResult{}, err
		}
	}
	pidFile := filepath.Join(filepath.Dir(spec.RootDir), fmt.Sprintf("exec-%d.pid", time.Now().UnixNano()))
	defer os.Remove(pidFile)

	args := []string{"exec", "--cwd", cwd, "--internal-pid-file", pidFile}
	for _, kv := range sandboxEnv(spec, req.Env) {
		args = append(args, "--env", kv)
	}
	args = append(args, containerID(spec), initInSandbox, "exec", "--")
	args = append(args, req.Argv...)

	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	c := exec.Command(r.binaryFor(spec), append(r.flags(spec), args...)...)
	c.Stdin, c.Stdout, c.Stderr = req.Stdin, req.Stdout, req.Stderr
	if err := c.Start(); err != nil {
		return ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()

	select {
	case err := <-done:
		res, err := exitResult(c, err)
		// runsc exits 128 when it loses the sandbox mid-command
		if err == nil && res.ExitCode == 128 && r.status(ctx, spec) != "running" {
			if req.Stderr != nil {
				fmt.Fprintln(req.Stderr, "cairn: the workspace was stopped by the host (most likely it ran out of memory) and has been restarted; running processes were lost")
			}
			res.ExitCode = 137
			if herr := r.heal(ctx, spec, "sandbox died during a command"); herr != nil {
				return res, herr
			}
		}
		return res, err
	case <-runCtx.Done():
		// kill the process inside the sandbox, then the client
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = r.run(context.Background(), spec, "kill", "--pid", strconv.Itoa(pid), containerID(spec), "KILL")
			}
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = c.Process.Kill()
			<-done
		}
		res := ExecResult{ExitCode: -1}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			res.TimedOut = true
			return res, nil
		}
		return res, ctx.Err()
	}
}

func exitResult(c *exec.Cmd, err error) (ExecResult, error) {
	var exitErr *exec.ExitError
	if err == nil || errors.As(err, &exitErr) {
		return ExecResult{ExitCode: c.ProcessState.ExitCode()}, nil
	}
	return ExecResult{}, err
}

// sandboxEnv is the environment of every command: a sane PATH and the
// workspace's identity, plus the caller's variables.
func sandboxEnv(spec Spec, extra map[string]string) []string {
	env := map[string]string{
		"PATH":               "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME":               "/root",
		"LANG":               "C.UTF-8",
		"HOSTNAME":           spec.Hostname,
		"CAIRN_WORKSPACE_ID": spec.ID,
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// --- limits -------------------------------------------------------------------

// cgroupPath is the sandbox's cgroup, relative to the engine's own cgroup root.
func cgroupPath(spec Spec) string { return "/cairn/" + containerID(spec) }

// hostMemoryMax is the host's hard limit for a workspace allowed limit bytes.
// The limit itself is enforced inside the sandbox (cairn-init); this is the
// backstop. There is deliberately no memory.high: memory the in-sandbox limit
// does not count (tmpfs, gVisor's own file cache) cannot be reclaimed by the
// host, and once it sits above memory.high every allocation is throttled, so
// the workspace hangs for good. Hitting memory.max instead kills the sandbox,
// which the next command restarts from its files.
func hostMemoryMax(limit int64) int64 { return limit + max(limit/2, 256<<20) }

// --- bundle -------------------------------------------------------------------

// writeBundle writes the OCI config for a workspace. Written fresh before every
// start and restore so limit changes and path moves (a copy) take effect.
func (r *Runsc) writeBundle(spec Spec) error {
	if spec.Network {
		// images exported from Docker carry an empty resolv.conf
		rc := filepath.Join(spec.RootDir, "etc", "resolv.conf")
		if data, err := os.ReadFile(rc); err != nil || len(bytes.TrimSpace(data)) == 0 {
			_ = os.MkdirAll(filepath.Dir(rc), 0o755)
			_ = os.Remove(rc) // may be a dangling symlink to the host's resolver
			if err := os.WriteFile(rc, []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0o644); err != nil {
				return err
			}
		}
	}
	dir := bundleDir(spec)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cfg := ociSpec(spec)
	// gVisor puts an in-memory tmpfs on /tmp unless told otherwise; files there
	// would miss quick saves and vanish if the engine restarts. Keep /tmp on the
	// workspace's disk instead, but only when it is a real directory: the bind
	// is made on the host, so it must never follow a link the workspace planted.
	tmpDir := filepath.Join(spec.RootDir, "tmp")
	if err := os.Mkdir(tmpDir, 0o1777); err == nil {
		_ = os.Chmod(tmpDir, 0o1777) // umask
	}
	mounts := cfg["mounts"].([]map[string]any)
	if fi, err := os.Lstat(tmpDir); err == nil && fi.IsDir() {
		mounts = append(mounts, map[string]any{
			"destination": "/tmp", "type": "bind", "source": tmpDir,
			"options": []string{"rbind", "rw", "nosuid", "nodev"},
		})
	}
	cfg["mounts"] = append(mounts, map[string]any{
		"destination": initInSandbox, "type": "bind", "source": initFile(spec),
		"options": []string{"bind", "ro"},
	})
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "config.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "config.json"))
}

func ociSpec(spec Spec) map[string]any {
	namespaces := []map[string]any{{"type": "pid"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"}}
	if spec.Network {
		namespaces = append(namespaces, map[string]any{"type": "network", "path": netnsPath(spec)})
	} else {
		namespaces = append(namespaces, map[string]any{"type": "network"})
	}
	resources := map[string]any{"pids": map[string]any{"limit": 4096}}
	initEnv := map[string]string{}
	if spec.Limits.MemoryMB > 0 {
		// The workspace's limit is enforced inside the sandbox by cairn-init. The
		// host limit is a backstop above it (the sandbox's own overhead, and room
		// for cairn-init to act): crossing it kills the whole sandbox.
		limit := spec.Limits.MemoryMB << 20
		initEnv["CAIRN_MEMORY_LIMIT"] = strconv.FormatInt(limit, 10)
		resources["memory"] = map[string]any{"limit": hostMemoryMax(limit)}
	}
	if spec.Limits.CPUs > 0 {
		const period = 100000
		resources["cpu"] = map[string]any{"quota": int64(spec.Limits.CPUs * period), "period": period}
	}
	return map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     []string{initInSandbox},
			"env":      sandboxEnv(spec, initEnv),
			"cwd":      "/",
			"capabilities": map[string]any{
				"bounding":  defaultCaps,
				"effective": defaultCaps,
				"permitted": defaultCaps,
			},
			"rlimits": []map[string]any{{"type": "RLIMIT_NOFILE", "hard": 65536, "soft": 65536}},
		},
		"root":     map[string]any{"path": spec.RootDir, "readonly": false},
		"hostname": spec.Hostname,
		"mounts": []map[string]any{
			{"destination": "/proc", "type": "proc", "source": "proc"},
			{"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{"destination": "/dev/shm", "type": "tmpfs", "source": "shm", "options": []string{"nosuid", "noexec", "nodev", "mode=1777", "size=268435456"}},
			{"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": []string{"nosuid", "noexec", "nodev", "ro"}},
		},
		"linux": map[string]any{
			"cgroupsPath": cgroupPath(spec),
			"namespaces":  namespaces,
			"resources":   resources,
		},
	}
}

// defaultCaps is the capability set Docker gives containers: enough for package
// managers and build tools; everything else (and the host) stays out of reach
// behind gVisor anyway.
var defaultCaps = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER", "CAP_MKNOD", "CAP_NET_RAW",
	"CAP_SETGID", "CAP_SETUID", "CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
}

// --- network ------------------------------------------------------------------
//
// A workspace with network on gets its own network namespace with a veth pair
// to the host and NAT to the internet. Each workspace has a /30 in
// 10.200.0.0/16, chosen from its ID, so the same workspace always gets the same
// addresses (a restore must see the network it was saved with). Cloud metadata
// and the host's own addresses are blocked.

func netnsName(spec Spec) string { return "cairn-" + shortHash(spec.ID, 10) }
func netnsPath(spec Spec) string { return "/var/run/netns/" + netnsName(spec) }

func shortHash(s string, n int) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return fmt.Sprintf("%016x", h.Sum64())[:n]
}

// subnet returns the /30 for a workspace: host side .1, workspace side .2.
func subnet(spec Spec) (hostIP, wsIP string) {
	h := fnv.New32a()
	h.Write([]byte(spec.ID))
	slot := h.Sum32()%16000 + 1 // 16000 slots of 4 addresses in 10.200.0.0/16
	base := slot * 4
	a, b := byte(base>>8), byte(base)
	return fmt.Sprintf("10.200.%d.%d", a, b+1), fmt.Sprintf("10.200.%d.%d", a, b+2)
}

func (r *Runsc) netUp(spec Spec) error {
	if !spec.Network {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, hostIf, wsIf := netnsName(spec), "ch"+shortHash(spec.ID, 10), "cw"+shortHash(spec.ID, 10)
	hostIP, wsIP := subnet(spec)
	sh := func(args ...string) error {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
		}
		return nil
	}
	_ = sh("ip", "netns", "add", ns) // exists already: fine
	if sh("ip", "-n", ns, "link", "show", wsIf) != nil {
		_ = sh("ip", "link", "del", hostIf)
		if err := sh("ip", "link", "add", hostIf, "type", "veth", "peer", "name", wsIf); err != nil {
			return err
		}
		if err := sh("ip", "link", "set", wsIf, "netns", ns); err != nil {
			return err
		}
	}
	// runsc strips the namespace on every start: re-apply everything
	steps := [][]string{
		{"ip", "addr", "replace", hostIP + "/30", "dev", hostIf},
		{"ip", "link", "set", hostIf, "up"},
		{"ip", "-n", ns, "addr", "flush", "dev", wsIf},
		{"ip", "-n", ns, "addr", "add", wsIP + "/30", "dev", wsIf},
		{"ip", "-n", ns, "link", "set", wsIf, "up"},
		{"ip", "-n", ns, "link", "set", "lo", "up"},
		{"ip", "-n", ns, "route", "replace", "default", "via", hostIP},
		{"sysctl", "-qw", "net.ipv4.ip_forward=1"},
	}
	for _, s := range steps {
		if err := sh(s...); err != nil {
			return err
		}
	}
	return r.firewallLocked(spec)
}

// firewall applies the host's rules for a workspace with network on.
func (r *Runsc) firewall(spec Spec) error {
	if !spec.Network {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firewallLocked(spec)
}

func (r *Runsc) firewallLocked(spec Spec) error {
	hostIf := "ch" + shortHash(spec.ID, 10)
	_, wsIP := subnet(spec)
	sh := func(args ...string) error {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
		}
		return nil
	}
	// What the workspace may reach. -I puts a rule first in its chain (ahead
	// of distribution defaults: Docker sets FORWARD to DROP).
	accepts := [][]string{
		{"-t", "nat", "-A", "POSTROUTING", "-s", wsIP + "/32", "!", "-o", hostIf, "-j", "MASQUERADE"},
		{"-I", "FORWARD", "-i", hostIf, "-j", "ACCEPT"},
		{"-I", "FORWARD", "-o", hostIf, "-j", "ACCEPT"},
	}
	for _, rule := range accepts {
		if sh(append([]string{"iptables"}, checkRule(rule)...)...) == nil {
			continue
		}
		if err := sh(append([]string{"iptables"}, rule...)...); err != nil {
			return err
		}
	}
	// What it may not, checked before the accepts: removed and put back at
	// position 1 every time, so they lead even where rules were left in
	// another order (engines before this fix inserted them below the accepts,
	// which let workspaces reach cloud metadata).
	for _, rule := range blockRules(hostIf) {
		for sh(append([]string{"iptables", "-D"}, rule...)...) == nil {
		}
		if err := sh(append([]string{"iptables", "-I", rule[0], "1"}, rule[1:]...)...); err != nil {
			return err
		}
	}
	return nil
}

// blockRules keep a workspace away from cloud metadata services (credentials
// for the host's cloud account live there) and from the host itself.
func blockRules(hostIf string) [][]string {
	return [][]string{
		{"FORWARD", "-i", hostIf, "-d", "169.254.0.0/16", "-j", "DROP"},     // AWS, GCP, Azure, Oracle, DigitalOcean...
		{"FORWARD", "-i", hostIf, "-d", "100.100.100.200/32", "-j", "DROP"}, // Alibaba Cloud
		{"INPUT", "-i", hostIf, "-j", "DROP"},                               // the host is not reachable
	}
}

// checkRule turns an add (-A/-I CHAIN ...) into the matching check (-C CHAIN ...).
func checkRule(rule []string) []string {
	out := append([]string{}, rule...)
	for i, a := range out {
		if a == "-A" || a == "-I" {
			out[i] = "-C"
			break
		}
	}
	return out
}

func (r *Runsc) netDown(spec Spec) {
	if !spec.Network {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	hostIf := "ch" + shortHash(spec.ID, 10)
	_, wsIP := subnet(spec)
	rules := [][]string{
		{"-t", "nat", "-D", "POSTROUTING", "-s", wsIP + "/32", "!", "-o", hostIf, "-j", "MASQUERADE"},
		{"-D", "FORWARD", "-i", hostIf, "-j", "ACCEPT"},
		{"-D", "FORWARD", "-o", hostIf, "-j", "ACCEPT"},
	}
	for _, b := range blockRules(hostIf) {
		rules = append(rules, append([]string{"-D"}, b...))
	}
	for _, rule := range rules {
		for exec.Command("iptables", rule...).Run() == nil { // every copy
		}
	}
	_ = exec.Command("ip", "link", "del", hostIf).Run()
	_ = exec.Command("ip", "netns", "del", netnsName(spec)).Run()
}
