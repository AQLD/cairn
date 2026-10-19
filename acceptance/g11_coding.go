package acceptance

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/oci/ocitest"
)

// G11: a coding agent's own workflow, driven only through the public client.
// The project is small but real: a Python app with a dependency served from a
// private package index (a wheelhouse in the workspace, as an internal mirror
// would be), a test suite, and a dev server.

// greetlibWheel is the project's one dependency, as a wheel pip installs with
// no network and no build tools.
var greetlibWheel = wheel("greetlib", "1.0", map[string]string{
	"greetlib/__init__.py": "def greet(name):\n    return f\"hello, {name}\"\n",
})

const appPy = `import http.server, sys

import greetlib


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = greetlib.greet("agent").encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
`

// The suite prints a line per test and pauses in the middle, so streamed output
// can be told apart from output that only arrives at the end.
const testAppPy = `import time, unittest

import greetlib


class TestGreet(unittest.TestCase):
    def test_1_greets(self):
        print("step 1", flush=True)
        self.assertEqual(greetlib.greet("a"), "hello, a")

    def test_2_slow(self):
        print("step 2", flush=True)
        time.sleep(1.5)

    def test_3_names(self):
        print("step 3", flush=True)
        self.assertTrue(greetlib.greet("b").endswith("b"))
`

// codingProject writes the project into a new workspace.
func codingProject(e *Env, name string) *client.Workspace {
	e.T.Helper()
	ws := e.Workspace(name)
	e.Must(e.C.WriteFile(e.Ctx, ws.ID, "wheelhouse/greetlib-1.0-py3-none-any.whl", greetlibWheel))
	e.Write(ws.ID, "requirements.txt", "greetlib==1.0\n")
	e.Write(ws.ID, "app.py", appPy)
	e.Write(ws.ID, "test_app.py", testAppPy)
	return ws
}

// installDeps creates the project's environment and installs requirements.txt
// from the wheelhouse only.
func installDeps(e *Env, ws string) {
	e.T.Helper()
	e.Sh(ws, "python3 -m venv .venv && .venv/bin/pip install -q --disable-pip-version-check --no-index --find-links wheelhouse -r requirements.txt")
}

// startDevServer starts app.py on port 8000 in the background (pid in
// server.pid).
func startDevServer(e *Env, ws string) {
	e.T.Helper()
	e.Sh(ws, "nohup .venv/bin/python app.py 8000 > server.log 2>&1 & echo $! > server.pid")
}

// fetchDevServer GETs the dev server from inside the workspace, waiting up to
// 10 s for it to listen.
func fetchDevServer(e *Env, ws string) string {
	e.T.Helper()
	fetch := "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8000', timeout=5).read().decode())"
	var body string
	var err error
	for i := 0; i < 100; i++ {
		if body, err = venvPython(e, ws, fetch); err == nil {
			return strings.TrimSpace(body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.T.Fatalf("dev server never answered: %v\nserver.log: %s", err, e.Read(ws, "server.log"))
	return ""
}

// venvPython is a one-line Python program run with the project's interpreter.
func venvPython(e *Env, ws, program string) (string, error) {
	return e.C.Run(e.Ctx, ws, ".venv/bin/python", "-c", program)
}

func init() {
	register("G11.1", func(e *Env) {
		// The team's image: its base toolchain (the suite's root filesystem when
		// one is set, as on gVisor) plus the team's own layer on top.
		reg := ocitest.New()
		defer reg.Close()
		reg.Auth, reg.Username, reg.Password = ocitest.Token, "ci-robot", "registry-secret"
		var layers [][]byte
		if tmpl := os.Getenv("CAIRN_TEST_TEMPLATE"); tmpl != "" {
			base, err := os.ReadFile(tmpl)
			e.Must(err)
			layers = append(layers, base)
		}
		layers = append(layers, ocitest.Layer(
			ocitest.Entry{Name: "etc/team/toolchain.conf", Body: "python=3\n"},
			ocitest.Entry{Name: "usr/local/bin/team-check", Mode: 0o755,
				Body: "#!/bin/sh\npython3 -c 'import sys, json; print(\"toolchain ready:\", sys.version_info.major)'\n"},
		))
		reg.Push("team/toolchain", "2026.10", false, layers...)

		start := time.Now()
		_, err := e.C.PullTemplate(e.Ctx, "team-toolchain", client.PullTemplate{
			Image: reg.Host() + "/team/toolchain:2026.10", Username: "ci-robot", Password: "registry-secret", Insecure: true})
		e.Must(err)
		pulled := time.Since(start)

		start = time.Now()
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "coding-toolchain", Template: "team-toolchain"})
		e.Must(err)
		out := e.Sh(ws.ID, "sh usr/local/bin/team-check")
		ready := time.Since(start)
		Equal(e, "the team's toolchain runs at once", strings.TrimSpace(out), "toolchain ready: 3")
		Equal(e, "the team's config is there", e.Read(ws.ID, "etc/team/toolchain.conf"), "python=3\n")
		e.T.Logf("image pulled in %v; workspace created and first command run in %v", pulled.Round(time.Millisecond), ready.Round(time.Millisecond))
	})

	register("G11.3.1", func(e *Env) {
		ws := codingProject(e, "coding-deps")
		installDeps(e, ws.ID)
		where, err := venvPython(e, ws.ID, "import greetlib; print(greetlib.__file__); print(greetlib.greet('agent'))")
		e.Must(err)
		lines := strings.Split(strings.TrimSpace(where), "\n")
		if len(lines) != 2 || !strings.Contains(lines[0], ".venv") {
			e.T.Fatalf("greetlib not installed into the project's environment: %q", where)
		}
		Equal(e, "the installed package works", lines[1], "hello, agent")
		// the system interpreter is untouched: the install went into .venv only
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"python3", "-c", "import greetlib"}})
		e.Must(err)
		if res.ExitCode == 0 {
			e.T.Fatal("greetlib is importable outside the project's environment")
		}
	})

	register("G11.3.2", func(e *Env) {
		ws := codingProject(e, "coding-tests")
		installDeps(e, ws.ID)
		start := time.Now()
		var mu sync.Mutex
		var out, errOut bytes.Buffer
		var firstStep time.Duration
		res, err := e.C.ExecStream(e.Ctx, ws.ID, client.Exec{Command: []string{".venv/bin/python", "-m", "unittest", "-v", "test_app"}},
			func(ev client.ExecEvent) {
				mu.Lock()
				defer mu.Unlock()
				if ev.Stream == "stdout" {
					out.Write(ev.Data)
					if firstStep == 0 && strings.Contains(out.String(), "step 1") {
						firstStep = time.Since(start)
					}
				} else {
					errOut.Write(ev.Data)
				}
			})
		e.Must(err)
		total := time.Since(start)
		Equal(e, "test suite exit code", res.ExitCode, 0)
		Equal(e, "every test reported", out.String(), "step 1\nstep 2\nstep 3\n")
		if !strings.Contains(errOut.String(), "OK") {
			e.T.Fatalf("unittest summary missing from stderr: %q", errOut.String())
		}
		// the slow test sleeps 1.5 s after "step 2": "step 1" must reach the
		// agent well before the run ends
		if firstStep == 0 || total-firstStep < time.Second {
			e.T.Fatalf("output did not stream: first line after %v, run took %v", firstStep, total)
		}
	})

	register("G11.3.3", func(e *Env) {
		ws := codingProject(e, "coding-devserver")
		installDeps(e, ws.ID)
		startDevServer(e, ws.ID)
		Equal(e, "dev server response", fetchDevServer(e, ws.ID), "hello, agent")

		// the agent does other work in between; the server is still there
		e.Sh(ws.ID, "ls > /dev/null")
		time.Sleep(time.Second)
		Equal(e, "dev server still serving", fetchDevServer(e, ws.ID), "hello, agent")
		e.Sh(ws.ID, "kill -0 $(cat server.pid)")
	})

	register("G11.4", func(e *Env) {
		ws := codingProject(e, "coding-overnight")
		installDeps(e, ws.ID)
		startDevServer(e, ws.ID)
		Equal(e, "dev server before the night", fetchDevServer(e, ws.ID), "hello, agent")
		pid := strings.TrimSpace(e.Read(ws.ID, "server.pid"))

		// end of the day: a full save stops the workspace, so it holds no CPU or memory
		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		w, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "workspace overnight", w.State, "stopped")
		time.Sleep(pauseFor())

		// next day: the same server answers, nothing was restarted or reinstalled
		_, err = e.C.Resume(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "dev server the next day", fetchDevServer(e, ws.ID), "hello, agent")
		e.Sh(ws.ID, "kill -0 "+pid)
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{".venv/bin/python", "-m", "unittest", "-q", "test_app"}})
		e.Must(err)
		Equal(e, "tests pass the next day", res.ExitCode, 0)
	})

	register("G11.6", func(e *Env) {
		ws := codingProject(e, "coding-undo")
		installDeps(e, ws.ID)
		startDevServer(e, ws.ID)
		Equal(e, "dev server before the command", fetchDevServer(e, ws.ID), "hello, agent")
		pid := strings.TrimSpace(e.Read(ws.ID, "server.pid"))
		before := stepSave(e, ws.ID) // the agent saves before each command

		// a clean-up command that goes too far: the project, its environment
		// and the dev server are gone
		e.Sh(ws.ID, "rm -rf .venv app.py test_app.py; kill "+pid+"; true")
		if _, err := e.C.ReadFile(e.Ctx, ws.ID, "app.py"); err == nil {
			e.T.Fatal("the destructive command did nothing")
		}

		e.Resume(before.ID) // one step back
		Equal(e, "the dev server after rewinding", fetchDevServer(e, ws.ID), "hello, agent")
		e.Sh(ws.ID, "kill -0 "+pid) // the same process, not a restart
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{".venv/bin/python", "-m", "unittest", "-q", "test_app"}})
		e.Must(err)
		Equal(e, "tests pass after rewinding", res.ExitCode, 0)
	})

	register("G11.5", func(e *Env) {
		ws := e.Workspace("coding-fix")
		e.Write(ws.ID, "calc.py", calcBuggy)
		e.Write(ws.ID, "test_calc.py", testCalcPy)
		// -B: no bytecode, so each copy runs exactly the source it has
		var lastOut string
		runTests := func(id string) int {
			res, err := e.C.Exec(e.Ctx, id, client.Exec{Command: []string{"python3", "-B", "-m", "unittest", "-q", "test_calc"}})
			e.Must(err)
			lastOut = res.Stdout + res.Stderr
			return res.ExitCode
		}
		if runTests(ws.ID) == 0 {
			e.T.Fatal("the bug does not show: the tests already pass")
		}

		sv := e.Save(ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, len(calcFixes))
		e.Must(err)
		Equal(e, "copies made", len(copies), len(calcFixes))

		// each copy tries one fix, all at once, while the original keeps
		// working on its own
		passed := make([]bool, len(copies))
		var wg sync.WaitGroup
		for i := range copies {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := e.C.WriteFile(e.Ctx, copies[i].ID, "calc.py", []byte(calcFixes[i])); err != nil {
					e.T.Error(err)
					return
				}
				res, err := e.C.Exec(e.Ctx, copies[i].ID, client.Exec{Command: []string{"python3", "-B", "-m", "unittest", "-q", "test_calc"}})
				if err != nil {
					e.T.Error(err)
					return
				}
				passed[i] = res.ExitCode == 0
			}(i)
		}
		originalStillFails := runTests(ws.ID) != 0
		wg.Wait()
		if e.T.Failed() {
			return
		}
		if !originalStillFails {
			e.T.Fatal("the original changed while its copies tried fixes")
		}
		var winners []int
		for i, ok := range passed {
			if ok {
				winners = append(winners, i)
			}
		}
		if len(winners) != 1 || winners[0] != calcRightFix {
			e.T.Fatalf("want only fix %d to pass, passed: %v", calcRightFix, winners)
		}
		for i, cp := range copies {
			Equal(e, fmt.Sprintf("copy %d kept its own fix", i), e.Read(cp.ID, "calc.py"), calcFixes[i])
		}
		Equal(e, "the original still has the bug", e.Read(ws.ID, "calc.py"), calcBuggy)

		// keep the winner: its change goes to the original, the copies go away
		e.Write(ws.ID, "calc.py", e.Read(copies[calcRightFix].ID, "calc.py"))
		if code := runTests(ws.ID); code != 0 {
			e.T.Fatalf("tests fail in the original after taking the winning fix (exit %d):\n%s", code, lastOut)
		}
		for _, cp := range copies {
			e.Must(e.C.DeleteWorkspace(e.Ctx, cp.ID))
			_, err := e.C.GetWorkspace(e.Ctx, cp.ID)
			e.ErrCode(err, "not_found")
		}
	})
}

const calcBuggy = "def average(xs):\n    return sum(xs) / (len(xs) + 1)\n"

const testCalcPy = `import unittest

from calc import average


class TestAverage(unittest.TestCase):
    def test_several(self):
        self.assertEqual(average([2, 4, 6]), 4)

    def test_fraction(self):
        self.assertEqual(average([1, 2]), 1.5)

    def test_one(self):
        self.assertEqual(average([5]), 5)

    def test_empty(self):
        self.assertEqual(average([]), 0)
`

// Three candidate fixes: one breaks on an empty list, one loses the fraction,
// one is right.
var calcFixes = []string{
	"def average(xs):\n    return sum(xs) / len(xs)\n",
	"def average(xs):\n    return sum(xs) // max(len(xs), 1)\n",
	"def average(xs):\n    return sum(xs) / len(xs) if xs else 0\n",
}

const calcRightFix = 2

// wheel builds a pure-Python wheel: the files plus the dist-info pip reads.
func wheel(name, version string, files map[string]string) []byte {
	info := fmt.Sprintf("%s-%s.dist-info/", name, version)
	all := map[string]string{
		info + "METADATA": fmt.Sprintf("Metadata-Version: 2.1\nName: %s\nVersion: %s\n", name, version),
		info + "WHEEL":    "Wheel-Version: 1.0\nGenerator: cairn-acceptance\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
	}
	for p, c := range files {
		all[p] = c
	}
	paths := make([]string, 0, len(all))
	for p := range all {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var record strings.Builder
	for _, p := range paths {
		sum := sha256.Sum256([]byte(all[p]))
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", p, base64.RawURLEncoding.EncodeToString(sum[:]), len(all[p]))
	}
	record.WriteString(info + "RECORD,,\n")
	all[info+"RECORD"] = record.String()
	paths = append(paths, info+"RECORD")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range paths {
		w, err := zw.Create(p)
		if err != nil {
			panic(err)
		}
		if _, err := w.Write([]byte(all[p])); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
