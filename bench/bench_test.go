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
	"os"
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

// BenchmarkFullSaveResume (G9.6): a full save of a workspace with a running
// process and some memory in use, then a resume from it, sub-benchmarked
// separately; and a wake from a pause (the same restore an idle workspace
// pays on its next command).
func BenchmarkFullSaveResume(b *testing.B) {
	setup := func(b *testing.B) (*acceptance.Env, string) {
		e := acceptance.NewEnv(b)
		if !e.Info.Capabilities.FullSave {
			b.Skip("needs full saves (gVisor)")
		}
		ws := e.Workspace("bench")
		fill(e, ws.ID, 16<<20)
		// a process holding ~64 MiB, like a language server
		e.Sh(ws.ID, `nohup python3 -c "b = bytearray(64 << 20); import time; time.sleep(1e9)" >/dev/null 2>&1 &`)
		return e, ws.ID
	}
	b.Run("save", func(b *testing.B) {
		e, ws := setup(b)
		for b.Loop() {
			if _, err := e.C.SaveWith(e.Ctx, ws, client.SaveOptions{Kind: client.FullSave, KeepRunning: true}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("resume", func(b *testing.B) {
		e, ws := setup(b)
		sv, err := e.C.Save(e.Ctx, ws, client.FullSave)
		if err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			e.Resume(sv.ID)
		}
	})
	b.Run("wake", func(b *testing.B) {
		e, ws := setup(b)
		for b.Loop() {
			b.StopTimer()
			if _, err := e.C.PauseWorkspace(e.Ctx, ws); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, err := e.C.Run(e.Ctx, ws, "true"); err != nil { // the next command wakes it
				b.Fatal(err)
			}
		}
	})
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
		"BenchmarkFullSaveResume":  true,
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

// BenchmarkImportTemplate: importing a template (CAIRN_TEST_AGENT_TOOLS, the
// big one, else CAIRN_TEST_TEMPLATE) through the API.
func BenchmarkImportTemplate(b *testing.B) {
	p := os.Getenv("CAIRN_TEST_AGENT_TOOLS")
	if p == "" {
		p = os.Getenv("CAIRN_TEST_TEMPLATE")
	}
	if p == "" {
		b.Skip("set CAIRN_TEST_AGENT_TOOLS or CAIRN_TEST_TEMPLATE")
	}
	e := acceptance.NewEnv(b)
	fi, err := os.Stat(p)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(fi.Size())
	i := 0
	for b.Loop() {
		f, err := os.Open(p)
		if err != nil {
			b.Fatal(err)
		}
		i++
		_, err = e.C.ImportTemplate(e.Ctx, fmt.Sprintf("imp%d", i), f)
		f.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}
