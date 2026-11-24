package acceptance

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AQLD/cairn/internal/engine"
)

// G5.9: secrets a workspace uses but never holds (docs/design/secrets.md).
// These scenarios cover how the engine keeps them.

// secretsKey writes a fresh key outside the engine's data folder and returns it.
func secretsKey(e *Env) []byte {
	e.T.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return key
}

// secretValue is a value no other test writes.
func secretValue() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "sv-" + hex.EncodeToString(b)
}

// holdsValue reports the first file under dir holding value (raw or base64).
func holdsValue(e *Env, dir, value string) string {
	e.T.Helper()
	needles := [][]byte{[]byte(value), []byte(base64.StdEncoding.EncodeToString([]byte(value)))}
	found := ""
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" || !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, n := range needles {
			if bytes.Contains(data, n) {
				found = p
			}
		}
		return nil
	})
	return found
}

func init() {
	register("G5.9.7", func(e *Env) {
		// without a key: refused, nothing stored
		_, err := e.C.SetSecret(e.Ctx, "API_KEY", secretValue(), []string{"api.example.com"})
		e.ErrCode(err, "unsupported_by_runtime")

		key := secretsKey(e)
		e.RestartWith(func(c *engine.Config) { c.SecretsKey = key })
		value := secretValue()
		sec, err := e.C.SetSecret(e.Ctx, "API_KEY", value, []string{"api.example.com", "*.example.org"})
		e.Must(err)
		Equal(e, "hosts", strings.Join(sec.Hosts, ","), "api.example.com,*.example.org")
		e.Restart() // flushes the store, writes it back from the WAL
		if p := holdsValue(e, e.DataDir, value); p != "" {
			e.T.Fatalf("the engine's data folder holds the secret's value in plaintext: %s", p)
		}
		list, err := e.C.ListSecrets(e.Ctx)
		e.Must(err)
		if len(list) != 1 || list[0].Name != "API_KEY" {
			e.T.Fatalf("after a restart with the same key: %+v", list)
		}

		// another key does not open them: the engine refuses to start with it
		e.stop()
		other := secretsKey(e)
		if eng, err := engine.Open(e.Ctx, engine.Config{DataDir: e.DataDir, Runtime: e.rt, SecretsKey: other, NoDiskImages: true}); err == nil {
			eng.Close()
			e.T.Fatal("the engine started with a key that does not open its secrets")
		} else if !strings.Contains(err.Error(), "secrets key") {
			e.T.Fatalf("refusal does not name the key: %v", err)
		}
		e.start()

		// bad requests
		_, err = e.C.SetSecret(e.Ctx, "9BAD", "v", []string{"x.example.com"})
		e.ErrCode(err, "invalid_argument")
		_, err = e.C.SetSecret(e.Ctx, "NO_HOSTS", "v", nil)
		e.ErrCode(err, "invalid_argument")
		e.Must(e.C.DeleteSecret(e.Ctx, "API_KEY"))
		e.ErrCode(e.C.DeleteSecret(e.Ctx, "API_KEY"), "not_found")
	})

	register("G5.9.5", func(e *Env) {
		key := secretsKey(e)
		var engineLog SyncBuffer
		e.RestartWith(func(c *engine.Config) {
			c.SecretsKey = key
			c.Logger = slog.New(slog.NewTextHandler(&engineLog, &slog.HandlerOptions{Level: slog.LevelDebug}))
		})
		value := secretValue()
		// through the CLI: the value on stdin, never in argv
		url, token := e.endpoint()
		cli := func(stdin string, args ...string) string {
			e.T.Helper()
			cmd := exec.CommandContext(e.Ctx, cairnCLI(e), args...)
			cmd.Env = append(os.Environ(), "CAIRN_ENDPOINT="+url, "CAIRN_TOKEN="+token, "CAIRN_CONFIG="+filepath.Join(e.T.TempDir(), "none.json"))
			cmd.Stdin = strings.NewReader(stdin)
			out, err := cmd.CombinedOutput()
			if err != nil {
				e.T.Fatalf("cairn %v: %v: %s", args, err, out)
			}
			return string(out)
		}
		out := cli(value+"\n", "secret", "set", "GITHUB_TOKEN", "--host", "github.com")
		shown := out + cli("", "secret", "ls")
		// and every API answer about it
		list, err := e.C.ListSecrets(e.Ctx)
		e.Must(err)
		sec, err := e.C.SetSecret(e.Ctx, "GITHUB_TOKEN", value, []string{"github.com"})
		e.Must(err)
		for _, s := range append(list, *sec) {
			shown += s.Name + strings.Join(s.Hosts, ",")
		}
		if !strings.Contains(shown, "GITHUB_TOKEN") {
			e.T.Fatalf("the secret is not listed:\n%s", shown)
		}
		for what, text := range map[string]string{"CLI and API output": shown, "API log": e.Logs.String(), "engine log": engineLog.String()} {
			if strings.Contains(text, value) {
				e.T.Fatalf("%s shows the secret's value", what)
			}
		}
		if !strings.Contains(engineLog.String(), "GITHUB_TOKEN") {
			e.T.Fatal("the engine log does not record the secret being set (by name)")
		}
		// an empty stdin is refused rather than storing nothing
		cmd := exec.CommandContext(e.Ctx, cairnCLI(e), "secret", "set", "EMPTY", "--host", "github.com")
		cmd.Env = append(os.Environ(), "CAIRN_ENDPOINT="+url, "CAIRN_TOKEN="+token)
		cmd.Stdin = strings.NewReader("")
		if out, err := cmd.CombinedOutput(); err == nil {
			e.T.Fatalf("secret set with nothing on stdin succeeded: %s", out)
		}
	})
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
