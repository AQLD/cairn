package acceptance

import (
	"bytes"
	"strconv"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G1: a coding agent picks up where it left off.

func init() {
	register("G1.1", func(e *Env) {
		ws := e.Workspace("agent")
		e.Write(ws.ID, "src/main.py", "print('hello')\n")
		e.Write(ws.ID, "notes/plan.md", "# plan\n- fix the bug\n")
		e.Sh(ws.ID, "mkdir -p build && head -c 65536 /dev/urandom > build/blob.bin")
		before := e.Sh(ws.ID, "cksum build/blob.bin")

		sv := e.Save(ws.ID)
		e.Sh(ws.ID, "rm -rf src notes build")
		e.Resume(sv.ID)

		Equal(e, "src/main.py", e.Read(ws.ID, "src/main.py"), "print('hello')\n")
		Equal(e, "notes/plan.md", e.Read(ws.ID, "notes/plan.md"), "# plan\n- fix the bug\n")
		Equal(e, "binary checksum", e.Sh(ws.ID, "cksum build/blob.bin"), before)
	})

	register("G1.2", func(e *Env) {
		ws := e.Workspace("tools")
		// an "installed tool": an executable script in a bin folder
		e.Sh(ws.ID, `mkdir -p tools/bin && printf '#!/bin/sh\necho tool-ok "$@"\n' > tools/bin/mytool && chmod 755 tools/bin/mytool`)
		sv := e.Save(ws.ID)
		e.Sh(ws.ID, "rm -rf tools")
		e.Resume(sv.ID)
		Equal(e, "tool output", e.Sh(ws.ID, "./tools/bin/mytool 1 2"), "tool-ok 1 2\n")
		Equal(e, "mode", strings.TrimSpace(e.Sh(ws.ID, `ls -l tools/bin/mytool | cut -c1-10`)), "-rwxr-xr-x")
	})

	register("G1.3", func(e *Env) {
		ws := e.Workspace("rollback")
		e.Write(ws.ID, "a.txt", "saved")
		sv := e.Save(ws.ID)
		e.Write(ws.ID, "a.txt", "changed after save")
		e.Write(ws.ID, "b.txt", "created after save")
		e.Resume(sv.ID)
		Equal(e, "a.txt", e.Read(ws.ID, "a.txt"), "saved")
		_, err := e.C.ReadFile(e.Ctx, ws.ID, "b.txt")
		e.ErrCode(err, "not_found")
	})

	register("G1.4", func(e *Env) {
		ws := e.Workspace("takeover")
		e.Sh(ws.ID, "mkdir -p out && echo 'patch applied' > out/result.txt")
		sv := e.Save(ws.ID)
		e.Resume(sv.ID)
		Equal(e, "read through API", e.Read(ws.ID, "out/result.txt"), "patch applied\n")
	})

	// G1.6: commands behave like a real shell.
	register("G1.6.1", func(e *Env) {
		ws := e.Workspace("exit")
		for _, code := range []int{0, 1, 2, 42, 255} {
			res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sh", "-c", "exit " + strconv.Itoa(code)}})
			e.Must(err)
			Equal(e, "exit code", res.ExitCode, code)
		}
	})

	register("G1.6.2", func(e *Env) {
		ws := e.Workspace("streams")
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sh", "-c", "echo out; echo err >&2"}})
		e.Must(err)
		Equal(e, "stdout", res.Stdout, "out\n")
		Equal(e, "stderr", res.Stderr, "err\n")
	})

	register("G1.6.3", func(e *Env) {
		ws := e.Workspace("stdin")
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"cat"}, Stdin: "piped input\n"})
		e.Must(err)
		Equal(e, "stdout", res.Stdout, "piped input\n")
	})

	register("G1.6.4", func(e *Env) {
		ws := e.Workspace("env")
		e.Sh(ws.ID, "mkdir -p sub/dir")
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{
			Command: []string{"sh", "-c", `echo "$GREETING"; pwd | sed 's|.*/||'`},
			Env:     map[string]string{"GREETING": "hi there"},
			Dir:     "sub/dir",
		})
		e.Must(err)
		Equal(e, "stdout", res.Stdout, "hi there\ndir\n")

		_, err = e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"pwd"}, Dir: "../.."})
		if err == nil {
			e.T.Fatal("working folder outside the workspace was accepted")
		}
	})

	register("G1.6.5", func(e *Env) {
		ws := e.Workspace("timeout")
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sleep", "30"}, TimeoutMS: 300})
		e.Must(err)
		Equal(e, "timed out", res.TimedOut, true)
		if res.DurationMS > 10_000 {
			e.T.Fatalf("timeout took %d ms", res.DurationMS)
		}
	})

	register("G1.6.6", func(e *Env) {
		ws := e.Workspace("bigout")
		// 6 MiB of output: more than the 4 MiB cap
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sh", "-c", "head -c 6291456 /dev/zero | tr '\\0' 'x'; echo done >&2"}, TimeoutMS: 60_000})
		e.Must(err)
		Equal(e, "exit code", res.ExitCode, 0)
		Equal(e, "truncated", res.StdoutTruncated, true)
		Equal(e, "kept bytes", len(res.Stdout), 4<<20)
		Equal(e, "stderr", res.Stderr, "done\n")
	})

	register("G1.6.8", func(e *Env) {
		ws := e.Workspace("stream")
		var stdout, stderr bytes.Buffer
		var firstAt time.Duration
		start := time.Now()
		res, err := e.C.ExecStream(e.Ctx, ws.ID, client.Exec{
			Command: []string{"sh", "-c", "echo first; sleep 1; echo second >&2; exit 3"},
		}, func(ev client.ExecEvent) {
			if firstAt == 0 {
				firstAt = time.Since(start)
			}
			if ev.Stream == "stdout" {
				stdout.Write(ev.Data)
			} else {
				stderr.Write(ev.Data)
			}
		})
		total := time.Since(start)
		e.Must(err)
		Equal(e, "exit code", res.ExitCode, 3)
		Equal(e, "stdout", stdout.String(), "first\n")
		Equal(e, "stderr", stderr.String(), "second\n")
		if firstAt == 0 || firstAt > total-500*time.Millisecond {
			e.T.Fatalf("first output arrived at %v of %v: not streamed", firstAt, total)
		}

		// errors before the command starts are ordinary API errors
		_, err = e.C.ExecStream(e.Ctx, "ws_doesnotexist", client.Exec{Command: []string{"true"}}, nil)
		e.ErrCode(err, "not_found")
	})

	register("G1.6.9", func(e *Env) {
		ws := e.Workspace("binary")
		e.Sh(ws.ID, "head -c 65536 /dev/urandom > blob.bin")
		want := e.Read(ws.ID, "blob.bin")

		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"cat", "blob.bin"}})
		e.Must(err)
		Equal(e, "non-streamed bytes", res.Stdout == want, true)

		var got bytes.Buffer
		_, err = e.C.ExecStream(e.Ctx, ws.ID, client.Exec{Command: []string{"cat", "blob.bin"}}, func(ev client.ExecEvent) {
			got.Write(ev.Data)
		})
		e.Must(err)
		Equal(e, "streamed bytes", got.String() == want, true)

		// text stays text
		res, err = e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"printf", "héllo ✓"}})
		e.Must(err)
		Equal(e, "utf-8 text", res.Stdout, "héllo ✓")
	})

	register("G1.6.7", func(e *Env) {
		ws := e.Workspace("notfound")
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"definitely-not-a-real-command-xyz"}})
		e.Must(err)
		Equal(e, "exit code", res.ExitCode, 127)
	})
}
