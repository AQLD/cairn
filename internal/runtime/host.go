package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Host runs commands directly on the host, inside the workspace folder.
//
// It is NOT a sandbox: a command can read and write anything the engine's user
// can. Use it only for development and for tests of the parts of Cairn that do
// not depend on isolation (API, state, save format, recovery).
type Host struct{}

func NewHost() *Host { return &Host{} }

func (*Host) Name() string    { return "host" }
func (*Host) Version() string { return "dev" }

func (*Host) Capabilities() Capabilities {
	return Capabilities{} // no isolation, limits, network control or memory saves
}

func (*Host) Start(_ context.Context, spec Spec) error {
	return os.MkdirAll(spec.RootDir, 0o755)
}

func (*Host) Stop(context.Context, Spec) error { return nil }

func (*Host) Checkpoint(context.Context, Spec, string) error { return ErrUnsupported }
func (*Host) Restore(context.Context, Spec, string) error    { return ErrUnsupported }

func (*Host) Exec(ctx context.Context, spec Spec, req ExecRequest) (ExecResult, error) {
	if len(req.Argv) == 0 {
		return ExecResult{}, errors.New("empty command")
	}
	dir := spec.RootDir
	if req.Dir != "" {
		clean := filepath.Clean(filepath.FromSlash(req.Dir))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return ExecResult{}, fmt.Errorf("working directory %q is outside the workspace", req.Dir)
		}
		dir = filepath.Join(spec.RootDir, clean)
	}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = hostEnv(spec, req.Env)
	cmd.Stdin = req.Stdin
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	if cmd.ProcessState == nil {
		// never started: report it the way a shell would
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			if req.Stderr != nil {
				fmt.Fprintf(req.Stderr, "%s: command not found\n", req.Argv[0])
			}
			return ExecResult{ExitCode: 127}, nil
		}
		return ExecResult{}, err
	}
	res := ExecResult{ExitCode: cmd.ProcessState.ExitCode()}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return res, nil
	}
	if err != nil {
		return ExecResult{}, err
	}
	return res, nil
}

// hostEnv gives commands a small, predictable environment instead of the
// engine's own, plus the workspace identity.
func hostEnv(spec Spec, extra map[string]string) []string {
	env := map[string]string{
		"PATH":               os.Getenv("PATH"),
		"HOME":               spec.RootDir,
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
