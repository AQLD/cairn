package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/AQLD/cairn/internal/engine"
)

func init() {
	// G10.10: the Python SDK's own test suite, run against this scenario's
	// engine, so the SDK is checked on every runtime the suite runs on
	// (including full saves on gVisor).
	register("G10.10", func(e *Env) {
		if e.Engine != nil { // in process: give it a secrets key, so secrets are covered too
			key := secretsKey(e)
			e.RestartWith(func(c *engine.Config) { c.SecretsKey = key })
		}
		endpoint, token := e.endpoint()
		cmd := exec.CommandContext(e.Ctx, "python3", "-W", "error", "-m", "unittest", "discover", "-v",
			"-s", filepath.Join("..", "sdk", "python", "tests"))
		cmd.Env = append(os.Environ(), "CAIRN_ENDPOINT="+endpoint, "CAIRN_TOKEN="+token)
		out, err := cmd.CombinedOutput()
		if err != nil {
			e.T.Fatalf("Python SDK tests failed: %v\n%s", err, out)
		}
		e.T.Logf("%s", out)
	})
}
