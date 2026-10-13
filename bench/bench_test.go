// Package bench measures the performance scenarios in spec (goal G9) through
// the public API, so numbers include everything a caller pays for.
//
//	go test -bench . -benchmem ./bench                    # all
//	go test -bench 'QuickSave' -benchtime 20x ./bench     # one, fixed iterations
//	CAIRN_TEST_RUNTIME=runsc go test -bench . ./bench     # on the Linux bench
//
// Benchmarks named in spec.Catalog but not written yet are listed in
// TestEveryBenchmarkScenarioIsCovered so they cannot be forgotten.
package bench

import (
	"fmt"
	"strings"
	"testing"

	"github.com/AQLD/cairn/acceptance"
	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/spec"
)

// sizes are filesystem sizes for save and resume benchmarks.
var sizes = []struct {
	name  string
	bytes int
}{
	{"1MiB", 1 << 20},
	{"16MiB", 16 << 20},
	{"128MiB", 128 << 20},
}

func BenchmarkCreateWorkspace(b *testing.B) {
	e := acceptance.NewEnv(b)
	for b.Loop() {
		if _, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExec(b *testing.B) {
	e := acceptance.NewEnv(b)
	ws := e.Workspace("bench")
	for b.Loop() {
		if _, err := e.C.Run(e.Ctx, ws.ID, "true"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQuickSave(b *testing.B) {
	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			e := acceptance.NewEnv(b)
			ws := e.Workspace("bench")
			fill(e, ws.ID, sz.bytes)
			b.SetBytes(int64(sz.bytes))
			for b.Loop() {
				e.Save(ws.ID)
			}
		})
	}
}

func BenchmarkResume(b *testing.B) {
	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			e := acceptance.NewEnv(b)
			ws := e.Workspace("bench")
			fill(e, ws.ID, sz.bytes)
			sv := e.Save(ws.ID)
			b.SetBytes(int64(sz.bytes))
			for b.Loop() {
				e.Resume(sv.ID)
			}
		})
	}
}

func BenchmarkCopy(b *testing.B) {
	for _, n := range []int{1, 5, 20} {
		b.Run(fmt.Sprintf("copies=%d", n), func(b *testing.B) {
			e := acceptance.NewEnv(b)
			ws := e.Workspace("bench")
			fill(e, ws.ID, 16<<20)
			sv := e.Save(ws.ID)
			for b.Loop() {
				if _, err := e.C.Copy(e.Ctx, sv.ID, n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// fill writes size bytes of incompressible data spread over a few files, like
// a small project with build output.
func fill(e *acceptance.Env, ws string, size int) {
	const files = 8
	per := size / files
	e.Sh(ws, fmt.Sprintf("mkdir -p data && for i in 1 2 3 4 5 6 7 8; do head -c %d /dev/urandom > data/f$i.bin; done", per))
}

// TestEveryBenchmarkScenarioIsCovered lists benchmark scenarios whose
// benchmark does not exist yet, so the gap is visible in every test run.
func TestEveryBenchmarkScenarioIsCovered(t *testing.T) {
	have := map[string]bool{
		"BenchmarkCreateWorkspace": true,
		"BenchmarkExec":            true,
		"BenchmarkQuickSave":       true,
		"BenchmarkResume":          true,
		"BenchmarkCopy":            true,
	}
	var missing []string
	spec.Walk(spec.Catalog, func(s spec.Scenario, _ []spec.Scenario) {
		if s.Bench != "" && !have[s.Bench] {
			missing = append(missing, s.ID+" "+s.Bench+" ("+string(s.Milestone)+")")
		}
	})
	if len(missing) > 0 {
		t.Skipf("benchmarks still to write: %s", strings.Join(missing, ", "))
	}
}
