package acceptance

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G7.12: a power cut mid-operation loses nothing the engine acknowledged.
//
// A kill -9 (G7.6) loses only the process: whatever it wrote is still in the
// kernel and reaches the disk. A power cut also loses what was written but not
// yet flushed, so this is the test of the engine's fsyncs. cairnd runs in a
// Lima VM of the dev environment (CAIRN_POWERCUT_VM, e.g. cairn-b); the test
// runs on the machine hosting it, drives the same random workload as G7.6, and
// cuts the VM's power (`limactl stop --force`) at a random moment.
//
//	CAIRN_POWERCUT_VM=cairn-b go test ./acceptance -run 'G7/G7.12' -v -timeout 60m

func init() { register("G7.12", powerCutTest) }

func powerCutTest(e *Env) {
	vm := os.Getenv("CAIRN_POWERCUT_VM")
	if vm == "" {
		e.T.Skip("needs CAIRN_POWERCUT_VM: a dev VM (devenv/dev.sh up) whose power the test may cut")
	}
	rounds := 3
	if v, err := strconv.Atoi(os.Getenv("CAIRN_POWERCUT_ROUNDS")); err == nil && v > 0 {
		rounds = v
	}
	pc := newPowerCut(e, vm)
	crashTest(e, pc, rounds)
}

type powerCut struct {
	vm, bin, init, template, data string
	port                          int
	c                             *client.Client
}

func newPowerCut(e *Env, vm string) *powerCut {
	e.T.Helper()
	arch, err := limaShell(vm, "uname", "-m")
	e.Must(err)
	goarch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[strings.TrimSpace(string(arch))]
	// the repo is mounted in the dev VMs at the same path: build into it
	root, err := exec.Command("go", "env", "GOMOD").Output()
	e.Must(err)
	out := filepath.Join(filepath.Dir(strings.TrimSpace(string(root))), "bin", "powercut")
	build := exec.Command("go", "build", "-o", out+"/", "github.com/AQLD/cairn/cmd/cairnd", "github.com/AQLD/cairn/cmd/cairn-init")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		e.T.Fatalf("build for %s: %v\n%s", vm, err, b)
	}
	pc := &powerCut{
		vm: vm, bin: filepath.Join(out, "cairnd"), init: filepath.Join(out, "cairn-init"),
		template: envOr("CAIRN_POWERCUT_TEMPLATE", "/var/lib/cairn-test/base.tar"),
		data:     "/var/lib/cairn-powercut", port: 7601,
	}
	// a fresh data folder (left by an earlier run: unmount, then remove)
	pc.sh(e, fmt.Sprintf(`pkill -x cairnd; sleep 1
		for m in $(awk '$5 ~ "^%[1]s" {print $5}' /proc/self/mountinfo | sort -r); do umount -l "$m"; done
		rm -rf %[1]s && mkdir -p %[1]s`, pc.data))
	return pc
}

func (p *powerCut) start(e *Env) *client.Client {
	e.T.Helper()
	p.sh(e, fmt.Sprintf(`setsid nohup %s --data %s/data --listen 127.0.0.1:%d --runtime runsc --init %s `+
		`--import-template base=%s --default-template base --pause-on-shutdown=false >> %s/cairnd.log 2>&1 < /dev/null &`,
		p.bin, p.data, p.port, p.init, p.template, p.data))
	p.c = client.New(fmt.Sprintf("http://127.0.0.1:%d", p.port), "") // Lima forwards the VM's localhost ports
	deadline := time.Now().Add(3 * time.Minute)
	for p.c.Health(e.Ctx) != nil {
		if time.Now().After(deadline) {
			log, _ := limaShell(p.vm, "sudo", "tail", "-20", p.data+"/cairnd.log")
			e.T.Fatalf("cairnd in %s did not come up:\n%s", p.vm, log)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return p.c
}

// crash cuts the VM's power: nothing is flushed, nothing shuts down.
func (p *powerCut) crash(e *Env) {
	e.T.Helper()
	if b, err := limactl("stop", "--force", p.vm).CombinedOutput(); err != nil {
		e.T.Fatalf("power cut %s: %v\n%s", p.vm, err, b)
	}
	if b, err := limactl("start", p.vm).CombinedOutput(); err != nil {
		e.T.Fatalf("power on %s: %v\n%s", p.vm, err, b)
	}
}

func (p *powerCut) stop(e *Env) {
	if p.c != nil {
		ws, _ := p.c.ListWorkspaces(e.Ctx)
		for _, w := range ws {
			_ = p.c.DeleteWorkspace(e.Ctx, w.ID)
		}
	}
	_, _ = limaShell(p.vm, "sudo", "pkill", "-x", "cairnd")
}

func (p *powerCut) dataDir() string       { return "" } // inside the VM
func (p *powerCut) window() time.Duration { return 6 * time.Second }

func (p *powerCut) sh(e *Env, script string) {
	e.T.Helper()
	if b, err := limaShell(p.vm, "sudo", "bash", "-c", script); err != nil {
		e.T.Fatalf("%s: %v\n%s", p.vm, err, b)
	}
}

// limaShell runs a command in the VM, retrying while ssh is not up yet (a VM
// just powered on), which limactl reports as exit status 255.
func limaShell(vm string, args ...string) ([]byte, error) {
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := limactl(append([]string{"shell", vm}, args...)...).CombinedOutput()
		var exit *exec.ExitError
		if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 255 || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(2 * time.Second)
	}
}

// limactl runs Lima's CLI, found on PATH or where devenv/dev.sh installs it.
func limactl(args ...string) *exec.Cmd {
	bin := "limactl"
	if _, err := exec.LookPath(bin); err != nil {
		if home, herr := os.UserHomeDir(); herr == nil {
			bin = filepath.Join(home, ".local", "lima", "bin", "limactl")
		}
	}
	return exec.Command(bin, args...)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
