package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// G17: a training run, driven the way training harnesses drive Cairn: a
// Python program using the Python SDK (testdata/training/harness.py), with a
// small reinforcement-learning environment in each workspace (env.py).

// endpoint is where the scenario's engine answers, for programs outside Go.
func (e *Env) endpoint() (url, token string) {
	if e.srv != nil {
		return e.srv.URL, e.token
	}
	return os.Getenv("CAIRN_ENDPOINT"), os.Getenv("CAIRN_TOKEN")
}

// harness runs one harness scenario and fails with its reason.
func harness(e *Env, scenario string) {
	e.T.Helper()
	url, token := e.endpoint()
	cmd := exec.CommandContext(e.Ctx, "python3", filepath.Join("testdata", "training", "harness.py"), scenario)
	cmd.Env = append(os.Environ(), "CAIRN_ENDPOINT="+url, "CAIRN_TOKEN="+token)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		e.T.Fatalf("training harness %s: %v\n%s", scenario, err, stderr.String())
	}
	e.T.Logf("%s: %s", scenario, strings.TrimSpace(string(out)))
}

func init() {
	register("G17.1", func(e *Env) { harness(e, "copy-live-setup") })
	register("G17.2", func(e *Env) { harness(e, "reset-between-episodes") })
}
