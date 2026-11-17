package acceptance

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// devMachine is a developer's machine as `cairn setup` sees it: its own home
// folder, fake coding-agent commands on PATH (the claude one records what it
// is asked and remembers an MCP registration), the real cairn CLI, and the
// scenario's engine.
type devMachine struct {
	e         *Env
	home, bin string
	cli       string
	endpoint  string
	token     string
}

func newDevMachine(e *Env, agents ...string) *devMachine {
	e.T.Helper()
	root := e.T.TempDir()
	m := &devMachine{e: e, home: filepath.Join(root, "home"), bin: filepath.Join(root, "bin")}
	m.endpoint, m.token = e.endpoint()
	e.Must(os.MkdirAll(m.home, 0o755))
	e.Must(os.MkdirAll(m.bin, 0o755))
	m.cli = filepath.Join(m.bin, "cairn")
	if out, err := exec.CommandContext(e.Ctx, "go", "build", "-o", m.cli, "../cmd/cairn").CombinedOutput(); err != nil {
		e.T.Fatalf("building the cairn CLI: %v\n%s", err, out)
	}
	fake := map[string]string{
		"claude": `#!/bin/sh
echo "$*" >> "$HOME/claude.log"
case "$1 $2" in
"mcp get") [ -f "$HOME/.claude-mcp" ]; exit $? ;;
"mcp add") touch "$HOME/.claude-mcp" ;;
esac
exit 0
`,
		"codex": "#!/bin/sh\nexit 0\n",
		"pi":    "#!/bin/sh\nexit 0\n",
	}
	for _, a := range agents {
		if a == "cursor" {
			e.Must(os.MkdirAll(filepath.Join(m.home, ".cursor"), 0o755))
			continue
		}
		e.Must(os.WriteFile(filepath.Join(m.bin, a), []byte(fake[a]), 0o755))
	}
	return m
}

// cairn runs the CLI in dir as this machine's user.
func (m *devMachine) cairn(dir string, args ...string) (string, error) {
	return m.cairnAt(m.endpoint, dir, args...)
}

func (m *devMachine) cairnAt(endpoint, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(m.e.Ctx, m.cli, args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + m.home, "PATH=" + m.bin + ":/usr/local/bin:/usr/bin:/bin",
		"CAIRN_ENDPOINT=" + endpoint, "CAIRN_TOKEN=" + m.token,
		"XDG_CONFIG_HOME=" + filepath.Join(m.home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (m *devMachine) mustCairn(dir string, args ...string) string {
	m.e.T.Helper()
	out, err := m.cairn(dir, args...)
	if err != nil {
		m.e.T.Fatalf("cairn %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (m *devMachine) read(rel string) string {
	m.e.T.Helper()
	data, err := os.ReadFile(filepath.Join(m.home, rel))
	if err != nil {
		m.e.T.Fatalf("%s: %v", rel, err)
	}
	return string(data)
}

// repo makes a git repository with a few files, in the machine's home.
func (m *devMachine) repo(name string) string {
	m.e.T.Helper()
	dir := filepath.Join(m.home, "code", name)
	for path, content := range map[string]string{
		"hello.txt":           "v1\n",
		"sub/dir/keep.txt":    "kept\n",
		"node_modules/x/x.js": "huge\n",
	} {
		p := filepath.Join(dir, path)
		m.e.Must(os.MkdirAll(filepath.Dir(p), 0o755))
		m.e.Must(os.WriteFile(p, []byte(content), 0o644))
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		m.e.T.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

func init() {
	register("G10.14.1", func(e *Env) {
		m := newDevMachine(e, "claude", "codex", "pi", "cursor")
		out := m.mustCairn(m.home, "setup")
		for _, want := range []string{"Claude Code:", "Codex:", "pi:", "Cursor:"} {
			if !strings.Contains(out, want) {
				e.T.Fatalf("setup did not connect %s:\n%s", strings.TrimSuffix(want, ":"), out)
			}
		}

		// the skill, where Claude Code and Codex/pi load skills
		claudeSkill := m.read(".claude/skills/cairn/SKILL.md")
		if !strings.HasPrefix(claudeSkill, "---\nname: cairn\n") || !strings.Contains(claudeSkill, "cairn save . --full") {
			e.T.Fatalf("Claude Code skill:\n%s", claudeSkill)
		}
		Equal(e, "skill for Codex and pi (~/.agents/skills)", m.read(".agents/skills/cairn/SKILL.md"), claudeSkill)

		// MCP, each in the agent's own way, pointing at this cairn
		Equal(e, "claude calls", m.read("claude.log"), "mcp get cairn\nmcp add --scope user cairn -- "+m.cli+" mcp\n")
		codex := m.read(".codex/config.toml")
		if !strings.Contains(codex, "[mcp_servers.cairn]") || !strings.Contains(codex, `command = "`+m.cli+`"`) {
			e.T.Fatalf("codex config:\n%s", codex)
		}
		if cursor := m.read(".cursor/mcp.json"); !strings.Contains(cursor, `"cairn"`) || !strings.Contains(cursor, m.cli) {
			e.T.Fatalf("cursor config:\n%s", cursor)
		}

		// again: nothing changes (paths in commands are relative to the
		// workspace's root, so they mean the same on every runtime)
		out = m.mustCairn(m.home, "setup")
		if strings.Count(out, "skill up to date") != 2 || !strings.Contains(out, "pi: skill shared") || strings.Count(out, "MCP already registered") != 3 {
			e.T.Fatalf("second setup was not a no-op:\n%s", out)
		}
		if n := strings.Count(m.read("claude.log"), "mcp add"); n != 1 {
			e.T.Fatalf("claude mcp add ran %d times", n)
		}
		if n := strings.Count(m.read(".codex/config.toml"), "[mcp_servers.cairn]"); n != 1 {
			e.T.Fatalf("codex config has %d cairn entries", n)
		}

		// a skill named cairn that setup did not write is never replaced
		own := "---\nname: cairn\ndescription: mine\n---\nmy notes\n"
		e.Must(os.WriteFile(filepath.Join(m.home, ".agents/skills/cairn/SKILL.md"), []byte(own), 0o644))
		out = m.mustCairn(m.home, "setup", "--agents", "pi")
		if !strings.Contains(out, "left as is") {
			e.T.Fatalf("setup did not say it left a foreign skill alone:\n%s", out)
		}
		Equal(e, "foreign skill", m.read(".agents/skills/cairn/SKILL.md"), own)

		// no agents: nothing written, and it says so
		bare := newDevMachine(e)
		out = bare.mustCairn(bare.home, "setup")
		if !strings.Contains(out, "none found") {
			e.T.Fatalf("setup with no agents:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(bare.home, ".agents")); err == nil {
			e.T.Fatal("setup wrote a skill with no agent installed")
		}
	})

	register("G10.14.2", func(e *Env) {
		m := newDevMachine(e, "claude")
		repo := m.repo("shop")
		e.Must(os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("# Rules\nkeep this\n"), 0o644))

		// from a subfolder: the repository is the project
		m.mustCairn(filepath.Join(repo, "sub"), "setup", "--project")
		pin, err := os.ReadFile(filepath.Join(repo, ".cairn", "workspace"))
		e.Must(err)
		ws := strings.TrimSpace(string(pin))
		w, err := e.C.GetWorkspace(e.Ctx, ws)
		e.Must(err)
		Equal(e, "workspace name", w.Name, "shop")
		Equal(e, "pushed file", e.Sh(ws, "cat root/shop/hello.txt"), "v1\n")
		Equal(e, "excluded node_modules", e.Sh(ws, "test -e root/shop/node_modules && echo there || echo absent"), "absent\n")
		Equal(e, "pin ignores itself", m.readAbs(filepath.Join(repo, ".cairn", ".gitignore")), "*\n")

		agents := m.readAbs(filepath.Join(repo, "AGENTS.md"))
		if !strings.HasPrefix(agents, "# Rules\nkeep this\n") || strings.Count(agents, "<!-- cairn:start -->") != 1 || !strings.Contains(agents, "/root/shop") {
			e.T.Fatalf("AGENTS.md:\n%s", agents)
		}
		Equal(e, "CLAUDE.md", m.readAbs(filepath.Join(repo, "CLAUDE.md")), "@AGENTS.md\n")

		// again: same workspace, one section
		out := m.mustCairn(repo, "setup", "--project")
		if !strings.Contains(out, "already set up") {
			e.T.Fatalf("second setup --project:\n%s", out)
		}
		Equal(e, "pin after rerun", strings.TrimSpace(m.readAbs(filepath.Join(repo, ".cairn", "workspace"))), ws)
		Equal(e, "AGENTS.md after rerun", m.readAbs(filepath.Join(repo, "AGENTS.md")), agents)

		// local edits run in the workspace, in the matching folder
		e.Must(os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("v2\n"), 0o644))
		Equal(e, "cairn run sees the edit", m.mustCairn(repo, "run", "--", "cat", "hello.txt"), "v2\n")
		if out := m.mustCairn(filepath.Join(repo, "sub", "dir"), "run", "--", "pwd"); !strings.HasSuffix(strings.TrimSpace(out), "/root/shop/sub/dir") {
			e.T.Fatalf("cairn run from sub/dir ran in %q", out)
		}
		Equal(e, "cairn exec . from a subfolder", m.mustCairn(filepath.Join(repo, "sub"), "exec", ".", "--", "cat", "root/shop/sub/dir/keep.txt"), "kept\n")
		if code := exitCode(m.cairn(repo, "run", "--", "sh", "-c", "exit 3")); code != 3 {
			e.T.Fatalf("cairn run exit code %d, want the command's 3", code)
		}

		// outside any project, "." is an error that says what to do
		out, err = m.cairn(m.home, "exec", ".", "--", "true")
		if err == nil || !strings.Contains(out, "setup --project") {
			e.T.Fatalf("cairn exec . outside a project: %v\n%s", err, out)
		}
	})

	register("G10.14.3", func(e *Env) {
		m := newDevMachine(e, "claude", "codex")
		repo := m.repo("app")
		m.mustCairn(repo, "setup", "--project")

		out, _ := m.cairn(repo, "doctor")
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "✗") && (strings.Contains(line, "Claude Code") || strings.Contains(line, "Codex") ||
				strings.Contains(line, "project") || strings.Contains(line, "AGENTS.md") || strings.Contains(line, "PATH")) {
				e.T.Fatalf("doctor found a problem in a fresh setup: %s\n%s", line, out)
			}
		}

		// each problem, with its fix
		e.Must(os.Remove(filepath.Join(m.home, ".agents/skills/cairn/SKILL.md")))
		out, err := m.cairn(repo, "doctor")
		if err == nil || !strings.Contains(out, "Codex: no skill") || !strings.Contains(out, "cairn setup --agents codex") {
			e.T.Fatalf("doctor with Codex's skill missing: %v\n%s", err, out)
		}

		// a skill from an older cairn: out of date, and setup updates it
		e.Must(os.MkdirAll(filepath.Join(m.home, ".agents/skills/cairn"), 0o755))
		e.Must(os.WriteFile(filepath.Join(m.home, ".agents/skills/cairn/SKILL.md"),
			[]byte("---\nname: cairn\n---\nold\n<!-- written by `cairn setup`; version 000000000000 -->\n"), 0o644))
		out, _ = m.cairn(repo, "doctor")
		if !strings.Contains(out, "Codex: skill out of date") {
			e.T.Fatalf("doctor with an old skill:\n%s", out)
		}
		if out := m.mustCairn(repo, "setup", "--agents", "codex"); !strings.Contains(out, "skill updated") {
			e.T.Fatalf("setup over an old skill:\n%s", out)
		}

		ws := strings.TrimSpace(m.readAbs(filepath.Join(repo, ".cairn", "workspace")))
		e.Must(e.C.DeleteWorkspace(e.Ctx, ws))
		out, _ = m.cairn(repo, "doctor")
		if !strings.Contains(out, ws) || !strings.Contains(out, "cairn setup --project") {
			e.T.Fatalf("doctor with the project's workspace gone:\n%s", out)
		}

		out, err = m.cairnAt("http://127.0.0.1:1", repo, "doctor")
		if err == nil || !strings.Contains(out, "no engine answers") || !strings.Contains(out, "cairn up") {
			e.T.Fatalf("doctor with no engine: %v\n%s", err, out)
		}
	})
}

func (m *devMachine) readAbs(path string) string {
	m.e.T.Helper()
	data, err := os.ReadFile(path)
	m.e.Must(err)
	return string(data)
}

func exitCode(_ string, err error) int {
	if err == nil {
		return 0
	}
	if x, ok := err.(*exec.ExitError); ok {
		return x.ExitCode()
	}
	return -1
}

// projectTemplate imports a template under name: the suite's own template
// where it has one (a real Linux with python3 on gVisor), else a tiny rootfs.
func projectTemplate(e *Env, name string) {
	e.T.Helper()
	var r io.Reader
	if path := os.Getenv("CAIRN_TEST_TEMPLATE"); path != "" {
		f, err := os.Open(path)
		e.Must(err)
		defer f.Close()
		r = f
	} else {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, d := range []string{"etc/", "tmp/", "root/"} {
			e.Must(tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755}))
		}
		e.Must(tw.Close())
		r = &buf
	}
	_, err := e.C.ImportTemplate(e.Ctx, name, r)
	e.Must(err)
}

// wheel builds a minimal pure-Python wheel, so a dependency installs with no
// network.
func localWheel(e *Env, dir, pkg, version string) string {
	e.T.Helper()
	name := fmt.Sprintf("%s-%s-py3-none-any.whl", pkg, version)
	f, err := os.Create(filepath.Join(dir, name))
	e.Must(err)
	zw := zip.NewWriter(f)
	info := fmt.Sprintf("%s-%s.dist-info/", pkg, version)
	files := map[string]string{
		pkg + "/__init__.py":   "VERSION = '" + version + "'\n",
		info + "METADATA":      "Metadata-Version: 2.1\nName: " + pkg + "\nVersion: " + version + "\n",
		info + "WHEEL":         "Wheel-Version: 1.0\nGenerator: cairn-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		info + "top_level.txt": pkg + "\n",
	}
	var record strings.Builder
	for path, content := range files {
		w, err := zw.Create(path)
		e.Must(err)
		_, err = w.Write([]byte(content))
		e.Must(err)
		h := sha256.Sum256([]byte(content))
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", path, base64.RawURLEncoding.EncodeToString(h[:]), len(content))
	}
	record.WriteString(info + "RECORD,,\n")
	w, err := zw.Create(info + "RECORD")
	e.Must(err)
	_, err = w.Write([]byte(record.String()))
	e.Must(err)
	e.Must(zw.Close())
	e.Must(f.Close())
	return name
}

func workspaceTemplate(e *Env, repo string) string {
	e.T.Helper()
	data, err := os.ReadFile(filepath.Join(repo, ".cairn", "workspace"))
	e.Must(err)
	w, err := e.C.GetWorkspace(e.Ctx, strings.TrimSpace(string(data)))
	e.Must(err)
	return w.Template
}

func init() {
	register("G10.14.4", func(e *Env) {
		projectTemplate(e, "dev-node")
		projectTemplate(e, "dev-python")
		projectTemplate(e, "mine")
		m := newDevMachine(e)

		node := m.repo("web")
		e.Must(os.WriteFile(filepath.Join(node, "package.json"), []byte(`{"name":"web"}`), 0o644))
		m.mustCairn(node, "setup", "--project", "--no-install")
		Equal(e, "template for package.json", workspaceTemplate(e, node), "dev-node")

		py := m.repo("api")
		e.Must(os.WriteFile(filepath.Join(py, "pyproject.toml"), []byte("[project]\nname='api'\n"), 0o644))
		m.mustCairn(py, "setup", "--project", "--no-install")
		Equal(e, "template for pyproject.toml", workspaceTemplate(e, py), "dev-python")

		plain := m.repo("notes")
		m.mustCairn(plain, "setup", "--project", "--no-install")
		Equal(e, "template with no marker", workspaceTemplate(e, plain), "dev-python")

		own := m.repo("own")
		e.Must(os.WriteFile(filepath.Join(own, "package.json"), []byte(`{}`), 0o644))
		m.mustCairn(own, "setup", "--project", "--no-install", "--template", "mine")
		Equal(e, "--template", workspaceTemplate(e, own), "mine")

		// what to install is still named for the agent
		if agents := m.readAbs(filepath.Join(node, "AGENTS.md")); !strings.Contains(agents, "cairn run -- npm install") {
			e.T.Fatalf("AGENTS.md for a node project with --no-install:\n%s", agents)
		}
	})

	register("G10.14.5", func(e *Env) {
		projectTemplate(e, "dev-python")
		m := newDevMachine(e)
		repo := m.repo("svc")
		whl := localWheel(e, repo, "tinydep", "1.0")
		e.Must(os.WriteFile(filepath.Join(repo, "requirements.txt"), []byte("./"+whl+"\n"), 0o644))

		out := m.mustCairn(repo, "setup", "--project")
		if !strings.Contains(out, "dependencies installed") {
			e.T.Fatalf("setup --project did not install the dependencies:\n%s", out)
		}
		Equal(e, "first run finds the dependency", m.mustCairn(repo, "run", "--", "python3", "-c", "import tinydep; print(tinydep.VERSION)"), "1.0\n")
		if agents := m.readAbs(filepath.Join(repo, "AGENTS.md")); !strings.Contains(agents, "already installed") {
			e.T.Fatalf("AGENTS.md does not say the dependencies are installed:\n%s", agents)
		}

		// unchanged dependency files: no second install
		out = m.mustCairn(repo, "setup", "--project")
		if !strings.Contains(out, "dependencies up to date") {
			e.T.Fatalf("rerun with the same dependencies:\n%s", out)
		}
		// changed: installed again
		whl2 := localWheel(e, repo, "tinydep", "2.0")
		e.Must(os.WriteFile(filepath.Join(repo, "requirements.txt"), []byte("./"+whl2+"\n"), 0o644))
		out = m.mustCairn(repo, "setup", "--project")
		if !strings.Contains(out, "dependencies installed") {
			e.T.Fatalf("rerun with changed dependencies:\n%s", out)
		}
		Equal(e, "new dependency", m.mustCairn(repo, "run", "--", "python3", "-c", "import tinydep; print(tinydep.VERSION)"), "2.0\n")

		// the baseline: dependencies in, later mess gone
		m.mustCairn(repo, "exec", ".", "--", "sh", "-c", "echo mess > root/svc/mess.txt; python3 -m pip uninstall -y -q tinydep")
		m.mustCairn(repo, "resume", ".")
		Equal(e, "after resume .", m.mustCairn(repo, "exec", ".", "--", "sh", "-c",
			"test -e root/svc/mess.txt && echo mess; python3 -c 'import tinydep; print(tinydep.VERSION)'"), "2.0\n")
	})

	register("G10.14.6", func(e *Env) {
		m := newDevMachine(e)
		repo := m.repo("report")
		m.mustCairn(repo, "setup", "--project", "--no-install")
		e.Must(os.WriteFile(filepath.Join(repo, "keep.txt"), []byte("mine\n"), 0o644))
		before, err := os.Stat(filepath.Join(repo, "hello.txt"))
		e.Must(err)

		out := m.mustCairn(repo, "run", "--", "sh", "-c",
			"mkdir -p out node_modules/x && echo result > out/report.md && echo junk > node_modules/x/j.js && echo printed")
		if !strings.HasPrefix(out, "printed\n") || !strings.Contains(out, "copied back 1 changed file(s): out/report.md") {
			e.T.Fatalf("cairn run output:\n%s", out)
		}
		Equal(e, "report copied back", m.readAbs(filepath.Join(repo, "out", "report.md")), "result\n")
		if _, err := os.Stat(filepath.Join(repo, "node_modules", "x", "j.js")); err == nil {
			e.T.Fatal("node_modules was copied back")
		}
		after, err := os.Stat(filepath.Join(repo, "hello.txt"))
		e.Must(err)
		Equal(e, "untouched file not rewritten", after.ModTime(), before.ModTime())

		// a command that changes a file the agent also has locally
		m.mustCairn(repo, "run", "--", "sh", "-c", "echo edited-there > keep.txt")
		Equal(e, "changed file copied back", m.readAbs(filepath.Join(repo, "keep.txt")), "edited-there\n")

		// --no-pull leaves results in the workspace
		m.mustCairn(repo, "run", "--no-pull", "--", "sh", "-c", "echo x > out/second.md")
		if _, err := os.Stat(filepath.Join(repo, "out", "second.md")); err == nil {
			e.T.Fatal("--no-pull copied a file back")
		}
	})
}
