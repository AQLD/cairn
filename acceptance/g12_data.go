package acceptance

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
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
}

// datasetRows: ~1 KiB rows; over 1 GiB in -short mode, about 3 GiB otherwise.
func datasetRows() int {
	if testing.Short() {
		return 1_100_000
	}
	return 3_100_000
}
