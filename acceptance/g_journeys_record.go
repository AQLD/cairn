package acceptance

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AQLD/cairn/client"
)

// G11.7 and G13.5: journey steps on the terminal (G19.3), diffs (G18.4) and
// the audit (G5.8).

// taggedTransport gives each request an ID the caller chooses (as a client
// tracing its own calls would) and remembers them.
type taggedTransport struct {
	mu   sync.Mutex
	n    int
	sent []string
}

func (t *taggedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.n++
	id := fmt.Sprintf("agent-req-%d", t.n)
	if r.Method != http.MethodGet {
		t.sent = append(t.sent, id)
	}
	t.mu.Unlock()
	r = r.Clone(r.Context())
	r.Header.Set("X-Request-Id", id)
	return http.DefaultTransport.RoundTrip(r)
}

func init() {
	register("G11.7", func(e *Env) {
		withAuth(e)
		agent, ka := keyClient(e, "coding-agent", "write")
		ws, err := agent.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "feature"})
		e.Must(err)
		e.Must(agent.WriteFile(e.Ctx, ws.ID, "app/main.py", []byte("print('v1')\n")))
		before, err := agent.Save(e.Ctx, ws.ID, client.QuickSave)
		e.Must(err)
		// the agent's step: it changes main.py and adds a broken config
		e.Must(agent.WriteFile(e.Ctx, ws.ID, "app/main.py", []byte("print('v2')\n")))
		e.Must(agent.WriteFile(e.Ctx, ws.ID, "app/config.ini", []byte("[broken\n")))
		after, err := agent.Save(e.Ctx, ws.ID, client.QuickSave)
		e.Must(err)

		// a person reviews what the step changed
		d, err := e.C.DiffSaves(e.Ctx, after.ID, before.ID, 100)
		e.Must(err)
		changes := map[string]string{}
		for _, c := range d.Changes {
			changes[strings.TrimPrefix(c.Path, "/")] = c.Change // diffs name paths from the workspace's root
		}
		if changes["app/main.py"] != "modified" || changes["app/config.ini"] != "added" {
			e.T.Fatalf("the review does not show the step's changes: %+v", d.Changes)
		}
		// and takes over in a terminal to fix the config
		t, sc := openTerm(e, e.C, ws.ID, 24, 80)
		typeLine(e, t, "printf '[app]\\nmode = fixed\\n' > /app/config.ini && echo fixed-by-hand")
		sc.waitFor(e, "fixed-by-hand")
		typeLine(e, t, "exit")
		select {
		case <-t.Done():
		case <-time.After(30 * time.Second):
			e.T.Fatal("the person's terminal did not end")
		}
		// the agent carries on from the person's fix
		out, err := agent.Run(e.Ctx, ws.ID, "cat", "/app/config.ini")
		e.Must(err)
		if !strings.Contains(out, "mode = fixed") {
			e.T.Fatalf("the agent does not see the person's fix: %q", out)
		}
		// the record tells them apart
		evs := auditOf(e, "")
		var person, agentRan bool
		for _, ev := range evs {
			if ev.WorkspaceID != ws.ID {
				continue
			}
			person = person || (ev.Kind == "terminal_open" && ev.Key == "operator")
			agentRan = agentRan || (ev.Kind == "exec" && ev.Key == ka.ID)
		}
		if !person || !agentRan {
			e.T.Fatalf("the record does not separate the person's terminal (%v) from the agent's commands (%v)", person, agentRan)
		}
	})

	register("G13.5", func(e *Env) {
		withAuth(e)
		k, err := e.C.CreateKey(e.Ctx, "browser-agent", "write")
		e.Must(err)
		url, _ := e.endpoint()
		tags := &taggedTransport{}
		agent := client.New(url, k.Token).WithHTTPClient(&http.Client{Transport: tags})
		ws, err := agent.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "browsing"})
		e.Must(err)
		e.Must(agent.WriteFile(e.Ctx, ws.ID, "page.html", []byte("<h1>order 42</h1>")))
		_, err = agent.Run(e.Ctx, ws.ID, "grep", "-o", "order [0-9]*", "page.html")
		e.Must(err)
		_, err = agent.ReadFile(e.Ctx, ws.ID, "page.html")
		e.Must(err)
		_, err = agent.Save(e.Ctx, ws.ID, client.QuickSave)
		e.Must(err)

		evs := auditOf(e, k.ID)
		recorded := map[string]string{}
		for _, ev := range evs {
			if ev.Key != k.ID {
				e.T.Fatalf("the key's audit holds another key's action: %+v", ev)
			}
			recorded[ev.RequestID] = ev.Kind
		}
		for _, kind := range []string{"create", "file_write", "exec", "file_read", "save"} {
			found := false
			for _, kd := range recorded {
				found = found || kd == kind
			}
			if !found {
				e.T.Fatalf("no %s by the agent's key on record: %+v", kind, evs)
			}
		}
		tags.mu.Lock()
		defer tags.mu.Unlock()
		for _, id := range tags.sent {
			if _, ok := recorded[id]; !ok {
				e.T.Fatalf("request %s the agent made is not on record (recorded: %v)", id, recorded)
			}
		}
	})
}
