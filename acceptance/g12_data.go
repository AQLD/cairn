package acceptance

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AQLD/cairn/client"
)

// G12: a data agent's own workflow, driven only through the public client:
// a large dataset streamed in, analysed in the workspace with Python, results
// streamed back out.

func init() {
	register("G12.1", func(e *Env) {
		ws := e.Workspace("data-load")
		rows, want := datasetRows(), int64(0)
		pr, pw := io.Pipe()
		go func() {
			payload := strings.Repeat("x", 1000)
			var err error
			for i := 0; i < rows && err == nil; i++ {
				amount := int64(i%997) - 400
				want += amount
				_, err = fmt.Fprintf(pw, "%d,%s,%d,%s\n", i, []string{"north", "south", "east", "west"}[i%4], amount, payload)
			}
			pw.CloseWithError(err)
		}()
		start := time.Now()
		e.Must(e.C.WriteFileFrom(e.Ctx, ws.ID, "data/sales.csv", pr))
		loaded := time.Since(start)

		start = time.Now()
		out := e.Sh(ws.ID, `python3 -c "
total = rows = 0
with open('data/sales.csv') as f:
    for line in f:
        total += int(line.split(',', 3)[2])
        rows += 1
print(rows, total)
"`)
		Equal(e, "the analysis sees every row and the exact total", strings.TrimSpace(out), fmt.Sprintf("%d %d", rows, want))
		e.T.Logf("%d rows (%d MiB): loaded in %v, analysed in %v", rows, int64(rows)*1030>>20, loaded.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	})

	register("G12.6", func(e *Env) {
		ws := e.Workspace("data-results")
		// the analysis writes a text table, a PNG chart and a large binary result
		e.Sh(ws.ID, `python3 -c "
import hashlib, os, struct, zlib
os.makedirs('out', exist_ok=True)
with open('out/summary.csv', 'w') as f:
    f.write('region,total\nnorth,120\nsouth,-45\neast,300\nwest,7\n')
w, h = 64, 32
raw = b''.join(b'\x00' + b''.join(bytes((x * 4 % 256, y * 8 % 256, 128)) for x in range(w)) for y in range(h))
def chunk(t, d):
    return struct.pack('>I', len(d)) + t + d + struct.pack('>I', zlib.crc32(t + d))
png = b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', w, h, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(raw)) + chunk(b'IEND', b'')
open('out/chart.png', 'wb').write(png)
with open('out/model.bin', 'wb') as f:
    for i in range(64):
        f.write(hashlib.sha256(str(i).encode()).digest() * 32768)
"`)
		sums := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(e.Sh(ws.ID, `python3 -c "
import hashlib
for name in ('summary.csv', 'chart.png', 'model.bin'):
    print(name, hashlib.sha256(open('out/' + name, 'rb').read()).hexdigest())
"`)), "\n") {
			f := strings.Fields(line)
			sums[f[0]] = f[1]
		}
		for _, name := range []string{"summary.csv", "chart.png", "model.bin"} {
			got := newCountingHash()
			e.Must(e.C.ReadFileTo(e.Ctx, ws.ID, "out/"+name, got))
			Equal(e, name+" comes back byte for byte", got.sum(), sums[name])
		}
		png, err := e.C.ReadFile(e.Ctx, ws.ID, "out/chart.png")
		e.Must(err)
		if !strings.HasPrefix(string(png), "\x89PNG\r\n\x1a\n") {
			e.T.Fatal("the chart is not a PNG")
		}
	})

	// G12.5: the session (a long-lived kernel holding the agent's state) and
	// its files outlive a query that asks for more memory than the workspace has.
	register("G12.5", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "data-oom", Limits: client.Limits{MemoryMB: 256}})
		e.Must(err)
		e.Sh(ws.ID, `mkdir -p data out && seq 1 200000 > data/rows.csv && echo "region,total" > out/summary.csv`)
		sums := e.Sh(ws.ID, "sha256sum data/rows.csv out/summary.csv")
		background(e, ws.ID, counterProgram) // the session
		session := running(e, ws.ID)

		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"python3", "-c", `
rows = open('data/rows.csv').read().split()
joined = []
while True:
    joined.append(bytearray(32 << 20))  # a cross join that never fits
`}, TimeoutMS: 60_000})
		e.Must(err)
		if res.ExitCode == 0 || res.TimedOut {
			e.T.Fatalf("the query should have been stopped for memory: exit %d, timed out %v", res.ExitCode, res.TimedOut)
		}
		if !strings.Contains(res.Stderr, "out of memory") {
			e.T.Fatalf("the query's error does not say it ran out of memory: %q", tail(res.Stderr, 300))
		}
		Equal(e, "the session's files", e.Sh(ws.ID, "sha256sum data/rows.csv out/summary.csv"), sums)
		Equal(e, "the same session carries on", running(e, ws.ID).PID, session.PID)
		Equal(e, "a smaller query then works", strings.TrimSpace(e.Sh(ws.ID, `python3 -c "print(sum(int(x) for x in open('data/rows.csv')))"`)), "20000100000")
	})

	// G12.2, G12.3: the agent loads the data into a Python session once and
	// asks its questions there, as in a notebook, today and a week later.
	register("G12.2", func(e *Env) {
		ws := e.Workspace("data-session")
		ses, loadedIn := loadSalesSession(e, ws.ID)
		for _, q := range salesQuestions {
			start := time.Now()
			Equal(e, q.code, cell(e, ws.ID, ses, q.code), q.want)
			if took := time.Since(start); took > loadedIn {
				e.T.Fatalf("%s took %v, longer than loading the data (%v): it was not kept loaded", q.code, took, loadedIn)
			}
		}
		Equal(e, "loads", cell(e, ws.ID, ses, "loads"), "1\n")
	})

	register("G12.3", func(e *Env) {
		ws := e.Workspace("data-week")
		ses, _ := loadSalesSession(e, ws.ID)
		Equal(e, salesQuestions[0].code, cell(e, ws.ID, ses, salesQuestions[0].code), salesQuestions[0].want)
		saved, resumed := pauseAndResume(e, ws.ID) // the week away, at no compute cost

		for _, q := range salesQuestions[1:] {
			Equal(e, q.code+" after the pause", cell(e, ws.ID, ses, q.code), q.want)
		}
		Equal(e, "loads", cell(e, ws.ID, ses, "loads"), "1\n")
		// the session knows how long it was away
		away, err := strconv.ParseFloat(strings.TrimSpace(cell(e, ws.ID, ses, "print(round(time.time() - loaded_at, 1))")), 64)
		e.Must(err)
		if pause := resumed.Sub(saved).Seconds(); away < pause {
			e.T.Fatalf("the session sees %.1fs since loading, less than the %.1fs pause", away, pause)
		}
	})
}

// salesQuestions are asked of the dataset loadSalesSession loads; the answers
// follow from how it is generated (amount = i % 997 - 400, region = i % 4).
var salesQuestions = []struct{ code, want string }{
	{"len(rows)", "1000000\n"},
	{"sum(a for _, a in rows)", fmt.Sprintf("%d\n", salesTotal(-1))},
	{"sum(a for r, a in rows if r == 'north')", fmt.Sprintf("%d\n", salesTotal(0))},
	{"max(a for r, a in rows if r == 'west')", "596\n"},
}

func salesTotal(region int) int64 {
	var t int64
	for i := 0; i < 1_000_000; i++ {
		if region < 0 || i%4 == region {
			t += int64(i%997 - 400)
		}
	}
	return t
}

// loadSalesSession writes a dataset into the workspace and loads it into a
// Python session; it returns the session and how long loading took.
func loadSalesSession(e *Env, ws string) (string, time.Duration) {
	e.T.Helper()
	e.Sh(ws, `python3 -c "
with open('/root/sales.csv', 'w') as f:
    for i in range(1_000_000):
        f.write('%d,%s,%d\n' % (i, ('north', 'south', 'east', 'west')[i % 4], i % 997 - 400))
"`)
	ses := session(e, ws, "python").ID
	start := time.Now()
	cell(e, ws, ses, `import csv, time
loads = 0
def load():
    global rows, loads, loaded_at
    with open('/root/sales.csv') as f:
        rows = [(r, int(a)) for _, r, a in csv.reader(f)]
    loads += 1
    loaded_at = time.time()
load()`)
	return ses, time.Since(start)
}

// datasetRows: ~1 KiB rows; over 1 GiB in -short mode, about 3 GiB otherwise.
func datasetRows() int {
	if testing.Short() {
		return 1_100_000
	}
	return 3_100_000
}
