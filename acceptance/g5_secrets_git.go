package acceptance

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/AQLD/cairn/client"
)

// G5.9.4, G5.9.6 and the journeys that use secrets (G11.2, G15.1).

// toolsSecretWorkspace creates a workspace from the agent-tools template
// (it has git) with the given secrets and allow-list.
func toolsSecretWorkspace(e *Env, name string, secrets, allow []string) *client.Workspace {
	e.T.Helper()
	f, err := os.Open(os.Getenv("CAIRN_TEST_AGENT_TOOLS"))
	e.Must(err)
	_, err = e.C.ImportTemplate(e.Ctx, "agent-tools", f)
	f.Close()
	e.Must(err)
	ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: name, Template: "agent-tools", Secrets: secrets, Egress: allow, Limits: client.Limits{DiskMB: 2048}})
	e.Must(err)
	return ws
}

// cloneWithSecret clones the test repository with the placeholder in the URL.
func cloneWithSecret(e *Env, ws string, w *secretWorld) {
	e.T.Helper()
	res, err := e.C.Exec(e.Ctx, ws, client.Exec{Command: []string{"sh", "-c",
		"git clone -q https://x:$GIT_TOKEN@" + w.allowedIP + ":8443/repo.git /root/code && cat /root/code/README.md"}, TimeoutMS: 60_000})
	e.Must(err)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "private code" {
		e.T.Fatalf("git clone with the secret: exit %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
}

func init() {
	register("G5.9.4", func(e *Env) {
		value := secretValue()
		w := newSecretWorldRepo(e, value)
		_, err := e.C.SetSecret(e.Ctx, "GIT_TOKEN", value, []string{w.allowedIP})
		e.Must(err)
		ws := toolsSecretWorkspace(e, "git", []string{"GIT_TOKEN"}, nil)
		cloneWithSecret(e, ws.ID, w)
		// git keeps the URL it cloned from: with the placeholder, never the value
		cfg := e.Sh(ws.ID, "cat /root/code/.git/config")
		if !strings.Contains(cfg, "cairn-secret-") {
			e.T.Fatalf("expected the placeholder in the remote URL:\n%s", cfg)
		}
		if p := scanFor(e, filepath.Join(e.DataDir, "workspaces", ws.ID), value, b64("x:"+value)); p != "" {
			e.T.Fatalf("the workspace's disk holds the credential: %s", p)
		}
		// and without the secret, the repository is private
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sh", "-c",
			"GIT_TERMINAL_PROMPT=0 git clone -q https://x:wrong@" + w.allowedIP + ":8443/repo.git /root/other"}, TimeoutMS: 60_000})
		e.Must(err)
		if res.ExitCode == 0 {
			e.T.Fatal("the repository cloned without the credential: the test server does not check it")
		}
	})

	register("G5.9.6", func(e *Env) {
		w := newSecretWorld(e)
		first := secretValue()
		_, err := e.C.SetSecret(e.Ctx, "API_TOKEN", first, []string{w.allowedIP})
		e.Must(err)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "rotated", Secrets: []string{"API_TOKEN"}})
		e.Must(err)
		auth := map[string]string{"Authorization": "Bearer $API_TOKEN"}
		if out, ok := call(e, ws.ID, "https://"+w.allowedIP+":8443/", auth); !ok || !strings.Contains(out, sha("Bearer "+first)) {
			e.T.Fatalf("before rotation: %s", out)
		}
		second := secretValue()
		_, err = e.C.SetSecret(e.Ctx, "API_TOKEN", second, []string{w.allowedIP})
		e.Must(err)
		if out, ok := call(e, ws.ID, "https://"+w.allowedIP+":8443/", auth); !ok || !strings.Contains(out, sha("Bearer "+second)) {
			e.T.Fatalf("after rotation the running workspace does not send the new value: %s", out)
		}
		e.Must(e.C.DeleteSecret(e.Ctx, "API_TOKEN"))
		before, _ := os.ReadFile(w.logs[w.allowedIP])
		if out, ok := call(e, ws.ID, "https://"+w.allowedIP+":8443/", auth); ok {
			e.T.Fatalf("a request with a deleted secret's placeholder went through: %s", out)
		}
		after, _ := os.ReadFile(w.logs[w.allowedIP])
		if len(after) != len(before) {
			e.T.Fatal("a request with a deleted secret reached the server")
		}
	})

	register("G11.2", func(e *Env) {
		value := secretValue()
		w := newSecretWorldRepo(e, value)
		_, err := e.C.SetSecret(e.Ctx, "GIT_TOKEN", value, []string{w.allowedIP})
		e.Must(err)
		// a coding agent's workspace: its git host only, and the credential for it
		ws := toolsSecretWorkspace(e, "coding", []string{"GIT_TOKEN"}, []string{w.allowedIP})
		cloneWithSecret(e, ws.ID, w)
		e.Sh(ws.ID, "cd /root/code && echo change >> README.md")
		e.Save(ws.ID)
		full, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		e.Resume(full.ID)
		for _, dir := range []string{filepath.Join(e.DataDir, "saves"), filepath.Join(e.DataDir, "workspaces", ws.ID)} {
			if p := scanFor(e, dir, value, b64("x:"+value)); p != "" {
				e.T.Fatalf("%s holds the credential", p)
			}
		}
		// the credential cannot leave for anywhere else: other hosts are off the allow-list
		if out, ok := call(e, ws.ID, "https://"+w.otherIP+":8443/", map[string]string{"Authorization": "Bearer $GIT_TOKEN"}); ok {
			e.T.Fatalf("the credential's placeholder reached another host: %s", out)
		}
	})

	register("G15.1", func(e *Env) {
		w := newSecretWorld(e)
		value := secretValue()
		_, err := e.C.SetSecret(e.Ctx, "INTERNAL_API_KEY", value, []string{w.allowedIP})
		e.Must(err)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "workflow", Secrets: []string{"INTERNAL_API_KEY"}})
		e.Must(err)
		if out, ok := call(e, ws.ID, "https://"+w.allowedIP+":8443/", map[string]string{"X-Api-Key": "$INTERNAL_API_KEY"}); !ok || !strings.Contains(out, sha(value)) {
			e.T.Fatalf("calling the internal API with its key: %s", out)
		}
		// nothing it can see holds the key: its environment, its processes, its files
		seen := e.Sh(ws.ID, "env; cat /proc/*/environ 2>/dev/null | tr '\\0' '\\n'; ls -R /run/cairn; cat /run/cairn/*.pem")
		if strings.Contains(seen, value) {
			e.T.Fatal("the workspace can read the key back")
		}
		// nor send it elsewhere
		if out, ok := call(e, ws.ID, "https://"+w.otherIP+":8443/", map[string]string{"X-Api-Key": "$INTERNAL_API_KEY"}); ok {
			e.T.Fatalf("the key's placeholder was sent to another host: %s", out)
		}
	})
}
