package acceptance

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/AQLD/cairn/client"
)

// G14: a research agent answers each question in a copy of one warm save:
// the tools are loaded once, every question gets its own workspace, and a
// finished question is deleted without a trace.

// researchTool stands in for a research agent's tools: it builds an index and
// then takes three seconds more, as loading a model would, before it answers
// questions on 127.0.0.1:7000. Each answer lists every question this process
// has answered, so copies that share anything show it.
const researchTool = `import json, os, socket, time
t0 = time.time()
index = {}
for d in range(20000):
    for w in ('w%d' % (d % 500), 'w%d' % (d % 7919)):
        index.setdefault(w, []).append(d)
time.sleep(3)
loaded = time.time()
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('127.0.0.1', 7000))
s.listen()
json.dump({'pid': os.getpid(), 'loaded': loaded, 'took': loaded - t0}, open('/tmp/tool.tmp', 'w'))
os.replace('/tmp/tool.tmp', '/tmp/tool.json')
answered = []
while True:
    c, _ = s.accept()
    q = c.recv(1024).decode().strip()
    answered.append(q)
    c.sendall(json.dumps({'pid': os.getpid(), 'loaded': loaded, 'docs': len(index.get(q, [])), 'answered': answered}).encode())
    c.close()
`

const askTool = `import socket, sys
c = socket.create_connection(('127.0.0.1', 7000), timeout=10)
c.sendall(sys.argv[1].encode())
c.shutdown(socket.SHUT_WR)
out = b''
while True:
    b = c.recv(65536)
    if not b:
        break
    out += b
print(out.decode())
`

type toolState struct {
	PID    int     `json:"pid"`
	Loaded float64 `json:"loaded"`
	Took   float64 `json:"took"`
}

type answer struct {
	toolState
	Docs     int      `json:"docs"`
	Answered []string `json:"answered"`
}

// researchDocs is how many documents the tool's index lists for q.
func researchDocs(q string) int {
	n := 0
	for d := 0; d < 20000; d++ {
		for _, w := range []string{fmt.Sprintf("w%d", d%500), fmt.Sprintf("w%d", d%7919)} {
			if w == q {
				n++
			}
		}
	}
	return n
}

// warmResearch starts the tools in a workspace and full-saves it once they
// are loaded.
func warmResearch(e *Env) (*client.Save, toolState) {
	e.T.Helper()
	ws := e.Workspace("research")
	e.Write(ws.ID, "tool.py", researchTool)
	e.Write(ws.ID, "ask.py", askTool)
	background(e, ws.ID, "exec python3 tool.py")
	var tool toolState
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		if b, err := e.C.ReadFile(e.Ctx, ws.ID, "tmp/tool.json"); err == nil && json.Unmarshal(b, &tool) == nil {
			break
		}
		if time.Now().After(deadline) {
			e.T.Fatal("the research tools did not load within a minute")
		}
	}
	warm, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
	e.Must(err)
	return warm, tool
}

// ask puts a question to the tools in a workspace.
func ask(e *Env, ws, q string) (answer, time.Duration) {
	e.T.Helper()
	start := time.Now()
	out := e.Sh(ws, "python3 ask.py "+q)
	took := time.Since(start)
	var a answer
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		e.T.Fatalf("answer to %s: %v: %q", q, err, out)
	}
	return a, took
}

// dataFootprint counts the folders and bytes the engine keeps for workspaces
// and saves.
func dataFootprint(e *Env) (dirs int, bytes int64) {
	e.T.Helper()
	for _, top := range []string{"workspaces", "saves"} {
		root := filepath.Join(e.DataDir, top)
		e.Must(filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if filepath.Dir(p) == root && d.IsDir() {
				dirs++
			}
			if d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					bytes += info.Size()
				}
			}
			return nil
		}))
	}
	return dirs, bytes
}

func init() {
	register("G14.1", func(e *Env) {
		warm, tool := warmResearch(e)
		copies, err := e.C.Copy(e.Ctx, warm.ID, 4)
		e.Must(err)
		for i, c := range copies {
			q := fmt.Sprintf("w%d", 3+i*101)
			a, took := ask(e, c.ID, q)
			Equal(e, "the loaded tool answers, not a new one", a.PID, tool.PID)
			Equal(e, "loaded once, before the save", a.Loaded, tool.Loaded)
			Equal(e, "documents found for "+q, a.Docs, researchDocs(q))
			if len(a.Answered) != 1 || a.Answered[0] != q {
				e.T.Fatalf("copy %d's tool has answered %v: copies share questions", i, a.Answered)
			}
			e.T.Logf("copy %d: answered in %v (loading took %.1fs)", i, took.Round(time.Millisecond), tool.Took)
		}
	})

	register("G14.5", func(e *Env) {
		warm, _ := warmResearch(e)
		question := func(i int) {
			copies, err := e.C.Copy(e.Ctx, warm.ID, 1)
			e.Must(err)
			ws := copies[0].ID
			ask(e, ws, fmt.Sprintf("w%d", i))
			// the work a question leaves in its workspace: notes and downloads
			e.Sh(ws, "mkdir -p notes && head -c 5000000 /dev/urandom > notes/download.bin && echo done > notes/answer.txt")
			e.Must(e.C.DeleteWorkspace(e.Ctx, ws))
		}

		question(0) // warm up: the first copy sets up what all of them share
		before := hostResources(e)
		listed, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		dirs, bytes := dataFootprint(e)
		for i := 1; i <= 5; i++ {
			question(i)
		}
		var after map[string]int
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			after = hostResources(e)
			if leaked(before, after) == "" || time.Now().After(deadline) {
				break
			}
		}
		if l := leaked(before, after); l != "" {
			e.T.Fatalf("after 5 questions: %s", l)
		}
		now, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces listed", len(now), len(listed))
		dirsAfter, bytesAfter := dataFootprint(e)
		Equal(e, "workspace and save folders on disk", dirsAfter, dirs)
		// a question writes 5 MB; a little slack for the engine's own records
		if bytesAfter > bytes+1<<20 {
			e.T.Fatalf("the engine keeps %d bytes more after 5 questions (%d -> %d)", bytesAfter-bytes, bytes, bytesAfter)
		}
		e.T.Logf("host: %v; data: %d folders, %d MiB", after, dirsAfter, bytesAfter>>20)
	})
}
