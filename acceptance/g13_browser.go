package acceptance

import (
	"bytes"
	"encoding/json"
	"image/png"
	"os"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G13: a browser agent signs in to a web app, waits for a person (a one-time
// code, an approval) and hands the page over, across pauses. The workspace
// comes from the agent-tools template (testdata/agent-tools): headless
// Chromium, a DevTools driver (cdp.py) and the web app (site.py), whose
// sessions live only in its memory and the browser's.

// toolsWorkspace creates a workspace from the agent-tools template.
func toolsWorkspace(e *Env, name string) *client.Workspace {
	e.T.Helper()
	f, err := os.Open(os.Getenv("CAIRN_TEST_AGENT_TOOLS"))
	e.Must(err)
	_, err = e.C.ImportTemplate(e.Ctx, "agent-tools", f)
	f.Close()
	e.Must(err)
	ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: name, Template: "agent-tools"})
	e.Must(err)
	return ws
}

// startBrowser starts the web app and the browser, and waits until the
// browser has a page to drive.
func startBrowser(e *Env, ws string) {
	e.T.Helper()
	background(e, ws, "exec /agent/browser.sh")
	waitFor(e, "the browser is up", 90*time.Second, func() bool {
		_, err := e.C.Run(e.Ctx, ws, "python3", "/agent/cdp.py", "eval", "1")
		return err == nil
	})
}

// cdp runs one DevTools command in the workspace, as the agent (or a person)
// connecting to the browser.
func cdp(e *Env, ws string, args ...string) string {
	e.T.Helper()
	out, err := e.C.Run(e.Ctx, ws, append([]string{"python3", "/agent/cdp.py"}, args...)...)
	e.Must(err)
	return strings.TrimSpace(out)
}

// pageText is what the browser's page shows.
func pageText(e *Env, ws string) string {
	e.T.Helper()
	var s string
	e.Must(json.Unmarshal([]byte(cdp(e, ws, "eval", "document.body.innerText")), &s))
	return s
}

// fill types into the form field named name.
func fill(e *Env, ws, name, value string) {
	e.T.Helper()
	sel, _ := json.Marshal("[name=" + name + "]")
	v, _ := json.Marshal(value)
	cdp(e, ws, "eval", "document.querySelector("+string(sel)+").value = "+string(v))
}

// enterPassword gets as far as the one-time code: the app has just sent it.
func enterPassword(e *Env, ws string) {
	e.T.Helper()
	cdp(e, ws, "goto", "http://127.0.0.1:8000/login")
	fill(e, ws, "user", "alice")
	fill(e, ws, "password", "correct horse")
	cdp(e, ws, "submit", "form", "/verify")
}

// enterCode reads the code the app sent the person and types it in.
func enterCode(e *Env, ws string) {
	e.T.Helper()
	code := strings.Fields(e.Read(ws, "root/outbox/code"))[0]
	fill(e, ws, "code", code)
	cdp(e, ws, "submit", "form", "/account")
}

// browserPIDs are the web app's and browser's process IDs.
func browserPIDs(e *Env, ws string) string {
	e.T.Helper()
	return strings.TrimSpace(e.Sh(ws, "cat /agent/run/site.pid /agent/run/browser.pid"))
}

func signedIn(e *Env, ws, when string) {
	e.T.Helper()
	if text := pageText(e, ws); !strings.Contains(text, "Signed in as alice") {
		e.T.Fatalf("%s: the page shows %q, not the signed-in account", when, text)
	}
}

func init() {
	register("G13.1", func(e *Env) {
		ws := toolsWorkspace(e, "browser-signed-in")
		startBrowser(e, ws.ID)
		enterPassword(e, ws.ID)
		enterCode(e, ws.ID)
		signedIn(e, ws.ID, "after signing in")
		pids := browserPIDs(e, ws.ID)

		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		time.Sleep(time.Second)
		e.Resume(sv.ID)

		Equal(e, "the same app and browser processes", browserPIDs(e, ws.ID), pids)
		// a fresh load: the browser sends its session cookie, the app knows it
		cdp(e, ws.ID, "goto", "http://127.0.0.1:8000/account")
		signedIn(e, ws.ID, "after the pause")
	})

	register("G13.2", func(e *Env) {
		ws := toolsWorkspace(e, "browser-code")
		startBrowser(e, ws.ID)
		enterPassword(e, ws.ID)
		sent := e.Read(ws.ID, "root/outbox/code")

		// the person has not answered yet: wait without holding compute
		sv := waitsForFree(e, ws.ID)
		time.Sleep(time.Second)
		e.Resume(sv.ID)

		Equal(e, "codes sent: still the one from before the pause", e.Read(ws.ID, "root/outbox/code"), sent)
		if text := pageText(e, ws.ID); !strings.Contains(text, "Enter the code") {
			e.T.Fatalf("after the pause the page shows %q, not the code form", text)
		}
		enterCode(e, ws.ID)
		signedIn(e, ws.ID, "after entering the code")
	})

	register("G13.3", func(e *Env) {
		ws := toolsWorkspace(e, "browser-handover")
		startBrowser(e, ws.ID)
		enterPassword(e, ws.ID)
		enterCode(e, ws.ID)
		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		time.Sleep(time.Second)
		e.Resume(sv.ID)

		// the person looks at the page the agent left...
		cdp(e, ws.ID, "shot", "/root/page.png")
		shot, err := e.C.ReadFile(e.Ctx, ws.ID, "root/page.png")
		e.Must(err)
		img, err := png.Decode(bytes.NewReader(shot))
		if err != nil {
			e.T.Fatalf("the screenshot is not a PNG: %v", err)
		}
		b := img.Bounds()
		colours := map[uint32]bool{}
		for y := b.Min.Y; y < b.Max.Y; y += 4 {
			for x := b.Min.X; x < b.Max.X; x += 4 {
				r, g, bl, _ := img.At(x, y).RGBA()
				colours[r>>8<<16|g>>8<<8|bl>>8] = true
			}
		}
		if b.Dx() < 200 || b.Dy() < 200 || len(colours) < 2 {
			e.T.Fatalf("the screenshot is %dx%d with %d colours: no page in it", b.Dx(), b.Dy(), len(colours))
		}
		signedIn(e, ws.ID, "what the person sees")

		// ...and takes over: their own connection, the agent's signed-in session
		cdp(e, ws.ID, "submit", "form", "/approve")
		if text := pageText(e, ws.ID); !strings.Contains(text, "approved by alice") {
			e.T.Fatalf("after the person's click the page shows %q", text)
		}
		Equal(e, "the app's record", e.Read(ws.ID, "root/approvals.log"), "refund 1042 approved by alice\n")
	})
}
