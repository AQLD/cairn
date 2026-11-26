package main

// cairn up --native: on Linux, cairnd straight on the host instead of in
// Docker. It runs as root (gVisor builds each sandbox's namespaces, cgroups
// and disk images), detached from the CLI, from the cairnd installed next to
// it. Everything it has lives in one folder cairn up made:
//
//	<dir>/up.json     what it was started with (token, port, template, binary)
//	<dir>/cairnd.pid  its process
//	<dir>/cairnd.log  its log
//	<dir>/engine/     its data folder (workspaces, saves, templates)
//
// up.json marks the folder as cairn up's: down --purge deletes nothing else.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AQLD/cairn/client"
)

// nativeRoot holds native engines' folders, one per --name.
const nativeRoot = "/var/lib/cairn-up"

// nativeStopWait is how long down waits for the engine to pause every
// workspace and exit (its own shutdown timeout is 2 minutes).
const nativeStopWait = 150 * time.Second

type nativeState struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	Template string `json:"template"`
	Token    string `json:"token"`
	Cairnd   string `json:"cairnd"`
	SHA256   string `json:"sha256"` // of cairnd: a new build replaces the engine
	Config   string `json:"config"` // the client config written for it
}

// nativeDir is the folder of a native engine named name (or the one given),
// and whether cairn up made it.
func nativeDir(name, data string) (string, bool) {
	dir := data
	if dir == "" {
		dir = filepath.Join(nativeRoot, name)
	}
	_, err := os.Stat(filepath.Join(dir, "up.json"))
	return dir, err == nil || data != ""
}

func readNativeState(dir string) (*nativeState, error) {
	b, err := os.ReadFile(filepath.Join(dir, "up.json"))
	if err != nil {
		return nil, err
	}
	var st nativeState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "up.json"), err)
	}
	return &st, nil
}

func writeNativeState(dir string, st *nativeState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "up.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "up.json"))
}

// nativePID is the running engine of dir, or 0. The process must be a
// cairnd serving dir, so a recycled pid is never taken for it.
func nativePID(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "cairnd.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !strings.Contains(string(cmdline), "\x00"+filepath.Join(dir, "engine")+"\x00") {
		return 0
	}
	return pid
}

// nativeByDefault says whether a plain `cairn up` runs the engine natively:
// on Linux, with gVisor and cairnd installed, as root, unless the engine of
// that name already runs in Docker or an image was asked for. Without root
// it says how to get there and leaves the engine in Docker.
func nativeByDefault(ctx context.Context, o *upOptions) bool {
	if _, err := os.Stat(filepath.Join(nativeRoot, o.name, "up.json")); err == nil {
		return true // brought up natively before
	}
	if o.set["image"] || o.build || !canRunNative() {
		return false
	}
	if dockerReady(ctx) == nil {
		if ct, _ := inspect(ctx, o.name); ct != nil {
			return false // the engine already runs in Docker: keep it there
		}
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "cairn: gVisor is installed here: `sudo cairn up` runs the engine without Docker")
		return false
	}
	return true
}

// canRunNative says whether this host could run the engine natively (Linux,
// gVisor and cairnd installed), root or not.
func canRunNative() bool {
	if goruntime.GOOS != "linux" {
		return false
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		return false
	}
	_, err := findBeside("cairnd")
	return err == nil
}

func upNative(ctx context.Context, o *upOptions) (int, error) {
	if goruntime.GOOS != "linux" {
		return 1, errors.New("--native runs the engine straight on Linux; here, drop --native to run it in Docker")
	}
	if os.Geteuid() != 0 {
		return 1, errors.New("--native needs root (gVisor makes namespaces, cgroups and disk images for each workspace): sudo cairn up --native")
	}
	cairnd, err := findBeside("cairnd")
	if err != nil {
		return 1, err
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cairnd), "cairn-init")); err != nil {
		if _, err := exec.LookPath("cairn-init"); err != nil {
			return 1, fmt.Errorf("cairn-init not found next to %s or on PATH (install.sh installs all three: cairn, cairnd, cairn-init)", cairnd)
		}
	}
	runsc, err := exec.LookPath("runsc")
	if err != nil {
		return 1, errors.New("runsc (gVisor) is not on PATH: install it (https://gvisor.dev/docs/user_guide/install/, or deploy/host-setup.sh in the Cairn repository)")
	}
	if o.template != "" {
		if n, v, ok := strings.Cut(o.template, "="); !ok || n == "" || v == "" {
			return 2, fmt.Errorf("--template: want NAME=IMAGE or NAME=ROOTFS.tar, got %q", o.template)
		}
	}
	dir, _ := nativeDir(o.name, o.data)
	if dir, err = filepath.Abs(dir); err != nil {
		return 1, err
	}
	sum, err := fileSHA256(cairnd)
	if err != nil {
		return 1, err
	}

	old, err := readNativeState(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			return 1, fmt.Errorf("%s exists and was not made by cairn up; pick another --data or --name", dir)
		}
		old = nil
	case err != nil:
		return 1, err
	}
	want := &nativeState{Name: o.name, Port: o.port, Template: o.template, Cairnd: cairnd, SHA256: sum, Config: configFor()}
	if old != nil {
		// unless asked otherwise, keep what the engine was started with
		if !o.set["port"] {
			want.Port = old.Port
		}
		if !o.set["template"] {
			want.Template = old.Template
		}
		want.Token = old.Token
	}
	if want.Token == "" {
		if cfg, err := client.ReadConfig(want.Config); err == nil && cfg.Endpoint == endpointFor(want.Port) {
			want.Token = cfg.Token // a purged engine on this port: keep the token clients have
		}
	}
	if want.Token == "" {
		want.Token = newToken()
	}

	fresh := false
	pid := nativePID(dir)
	switch {
	case pid != 0 && old != nil && old.Port == want.Port && old.Template == want.Template && old.Cairnd == want.Cairnd && old.SHA256 == want.SHA256:
		fmt.Fprintf(os.Stderr, "cairn: engine %q already running (pid %d)\n", o.name, pid)
	default:
		if pid != 0 {
			fmt.Fprintf(os.Stderr, "cairn: replacing engine %q (new binary or settings), keeping its workspaces\n", o.name)
			if err := stopNative(dir, pid); err != nil {
				return 1, err
			}
		} else if old != nil {
			fmt.Fprintf(os.Stderr, "cairn: starting engine %q (its workspaces wake on first use)\n", o.name)
		}
		fresh = old == nil || old.Template != want.Template
		// recorded before it starts, so down and purge always find it
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return 1, err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return 1, err
		}
		if err := writeNativeState(dir, want); err != nil {
			return 1, err
		}
		if err := startNative(dir, want, runsc); err != nil {
			return 1, err
		}
	}

	endpoint := endpointFor(want.Port)
	if err := writeUserConfig(want.Config, client.Config{Endpoint: endpoint, Token: want.Token}); err != nil {
		return 1, fmt.Errorf("writing %s: %w", want.Config, err)
	}
	c := client.New(endpoint, want.Token)
	if err := waitHealthy(ctx, endpoint, 90*time.Second); err != nil {
		return 1, fmt.Errorf("%w\nengine log (%s):\n%s", err, filepath.Join(dir, "cairnd.log"), tailFile(filepath.Join(dir, "cairnd.log"), 20))
	}
	info, err := c.Info(ctx)
	if err != nil {
		return 1, err
	}
	if !info.Capabilities.Isolation || !info.Capabilities.FullSave {
		fmt.Fprintf(os.Stderr, "cairn: warning: runtime %s has no isolation or full saves here\n", info.Runtime)
	}
	if tname, _, ok := strings.Cut(want.Template, "="); ok && fresh {
		logs := func() string { return tailFile(filepath.Join(dir, "cairnd.log"), 200) }
		if err := waitTemplate(ctx, c, logs, tname, 10*time.Minute); err != nil {
			return 1, err
		}
	}
	fmt.Printf("Cairn is running at %s (%s, native, gVisor %s)\n", endpoint, o.name, info.RuntimeVersion)
	fmt.Printf("config: %s\nengine folder: %s\n\n", want.Config, dir)
	fmt.Println("connect your coding agent (Claude Code, Codex, pi, Cursor), in your project:")
	fmt.Println("  cairn setup --project")
	fmt.Println("stop with `sudo cairn down` (workspaces and their processes are kept)")
	return 0, nil
}

// startNative starts cairnd for dir in its own session, so it outlives the
// CLI, its terminal and a Ctrl-C there.
func startNative(dir string, st *nativeState, runsc string) error {
	logf, err := os.OpenFile(filepath.Join(dir, "cairnd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	args := []string{"--listen", fmt.Sprintf("127.0.0.1:%d", st.Port), "--data", filepath.Join(dir, "engine"),
		"--runsc", runsc, "--shutdown-timeout", "2m"}
	if tname, v, ok := strings.Cut(st.Template, "="); ok {
		if strings.HasSuffix(v, ".tar") {
			if _, err := os.Stat(v); err != nil {
				return fmt.Errorf("--template %s: %w", st.Template, err)
			}
			args = append(args, "--import-template", st.Template)
		} else {
			args = append(args, "--pull-template", st.Template)
		}
		args = append(args, "--default-template", tname)
	}
	cmd := exec.Command(st.Cairnd, args...)
	env := []string{"CAIRN_TOKEN=" + st.Token} // from the environment, never argv (ps shows argv)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CAIRN_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", st.Cairnd, err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	if err := os.WriteFile(filepath.Join(dir, "cairnd.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cairn: started engine %q (pid %d) from %s\n", st.Name, pid, st.Cairnd)
	return nil
}

// stopNative asks the engine to stop (it pauses every workspace first) and
// waits until it has.
func stopNative(dir string, pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stopping engine (pid %d): %w", pid, err)
	}
	deadline := time.Now().Add(nativeStopWait)
	for nativePID(dir) == pid {
		if time.Now().After(deadline) {
			return fmt.Errorf("engine (pid %d) still running %s after being asked to stop; see %s", pid, nativeStopWait, filepath.Join(dir, "cairnd.log"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = os.Remove(filepath.Join(dir, "cairnd.pid"))
	return nil
}

func downNative(_ context.Context, dir string, purge bool) (int, error) {
	st, err := readNativeState(dir)
	if errors.Is(err, os.ErrNotExist) {
		if purge {
			if _, serr := os.Stat(dir); serr == nil {
				return 1, fmt.Errorf("%s was not made by cairn up; not deleting it", dir)
			}
		}
		fmt.Fprintf(os.Stderr, "cairn: no engine in %s\n", dir)
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	if pid := nativePID(dir); pid != 0 {
		fmt.Fprintf(os.Stderr, "cairn: stopping %q (pausing its workspaces first)\n", st.Name)
		if err := stopNative(dir, pid); err != nil {
			return 1, err
		}
	}
	if !purge {
		fmt.Fprintln(os.Stderr, "cairn: stopped; `cairn up --native` starts it again with every workspace")
		return 0, nil
	}
	// a stopped engine leaves nothing mounted; if it did (a crash), unmount
	// before deleting, so nothing is deleted through a mount
	if err := unmountUnder(dir); err != nil {
		return 1, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return 1, err
	}
	if cfg, err := client.ReadConfig(st.Config); err == nil && cfg.Endpoint == endpointFor(st.Port) {
		_ = os.Remove(st.Config)
	}
	fmt.Fprintf(os.Stderr, "cairn: removed %q and all its workspaces and saves\n", st.Name)
	return 0, nil
}

// describeNative is cairn status's line for a native engine.
func describeNative(dir string) {
	st, err := readNativeState(dir)
	if err != nil {
		return
	}
	if pid := nativePID(dir); pid != 0 {
		fmt.Printf("engine %s: native, pid %d, folder %s, %s\n", st.Name, pid, dir, st.Cairnd)
	} else {
		fmt.Printf("engine %s: native, stopped, folder %s\n", st.Name, dir)
	}
}

// unmountUnder unmounts everything mounted below dir, deepest first.
func unmountUnder(dir string) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil // not Linux: nothing of ours is mounted
	}
	defer f.Close()
	var points []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 {
			p := unescapeMount(fields[4])
			if p == dir || strings.HasPrefix(p, dir+"/") {
				points = append(points, p)
			}
		}
	}
	sort.Slice(points, func(i, j int) bool { return len(points[i]) > len(points[j]) })
	for _, p := range points {
		if err := syscall.Unmount(p, 0); err != nil && !errors.Is(err, syscall.EINVAL) {
			return fmt.Errorf("%s is still mounted (%v); not deleting %s", p, err, dir)
		}
	}
	return nil
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// findBeside finds a binary installed next to this one, then on PATH.
func findBeside(name string) (string, error) {
	if self, err := os.Executable(); err == nil {
		if self, err = filepath.EvalSymlinks(self); err == nil {
			p := filepath.Join(filepath.Dir(self), name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found next to cairn or on PATH (install.sh installs it)", name)
	}
	return filepath.Abs(p)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// configFor is the client config to write: under sudo, the invoking user's
// (their agents and CLI are the ones that connect), not root's.
func configFor() string {
	if os.Getenv("CAIRN_CONFIG") != "" || os.Getenv("SUDO_USER") == "" {
		return client.ConfigPath()
	}
	u, err := user.Lookup(os.Getenv("SUDO_USER"))
	if err != nil || u.HomeDir == "" {
		return client.ConfigPath()
	}
	return filepath.Join(u.HomeDir, ".config", "cairn", "config.json")
}

// writeUserConfig writes the config and, under sudo, gives it (and the
// folders made for it) to the invoking user.
func writeUserConfig(path string, cfg client.Config) error {
	if err := client.WriteConfig(path, cfg); err != nil {
		return err
	}
	if os.Getenv("CAIRN_CONFIG") != "" {
		return nil
	}
	uid, err1 := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err1 != nil || err2 != nil || uid == 0 {
		return nil
	}
	for _, p := range []string{filepath.Dir(filepath.Dir(path)), filepath.Dir(path), path} {
		if fi, err := os.Stat(p); err == nil {
			if sys, ok := fi.Sys().(*syscall.Stat_t); ok && sys.Uid == 0 {
				_ = os.Chown(p, uid, gid)
			}
		}
	}
	return nil
}
