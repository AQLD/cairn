package acceptance

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/AQLD/cairn/spec"
)

// Impl maps a scenario ID to the code that proves it. Register implementations
// from init functions with register.
var Impl = map[string]func(*Env){}

func register(id string, fn func(*Env)) {
	if _, dup := Impl[id]; dup {
		panic("acceptance: scenario implemented twice: " + id)
	}
	Impl[id] = fn
}

// Run walks the catalog as a tree of subtests. A scenario with code runs it;
// one without is skipped as pending (unless it only groups its children); one
// whose needs the environment lacks is skipped with the reason. Nothing is ever
// passed without running.
func Run(t *testing.T) {
	for _, s := range spec.Catalog {
		runScenario(t, s)
	}
}

// exclusive lists scenarios that need the machine to themselves when
// scenarios run in parallel (CAIRN_TEST_PARALLEL): they count host resources,
// fill the host's disk or memory, measure time or throughput, or kill
// engines. A listed ID covers its children; a family holding one is
// exclusive too. Go runs a goal's exclusive scenarios before its parallel
// ones start, and goals one after another, so these never overlap anything.
var exclusive = []string{
	"G1.7",   // golden workloads: saved within seconds of starting
	"G1.8",   // clocks across a pause
	"G3.2",   // CPU, memory and disk limits
	"G6.4.3", // the host volume really fills
	"G7.6",   // kill -9 rounds
	"G7.10",  // concurrent workers
	"G7.12",  // power cuts
	"G7.13",  // request floods
	"G8.5",   // a full workspace disk
	"G9.7",   // density
	"G9.8",   // a stopped workspace's cost
	"G9.9.5", // takes a workspace's mounts away (a host restart)
	"G9.9.9", // the host volume really fills
	"G10.6",  // upgrade: engines as processes
	"G10.7",  // counts host resources
	"G10.11", // Docker
	"G12.1",  // gigabytes of data
	"G13",    // browser saves of ~1 GB each: in parallel they fill the disk
	"G14.5",  // counts host resources
	"G19.5",  // gigabytes of files, free-space headroom
}

func isExclusive(s spec.Scenario) bool {
	for _, x := range exclusive {
		if s.ID == x || strings.HasPrefix(s.ID, x+".") {
			return true
		}
	}
	for _, c := range s.Children {
		if isExclusive(c) {
			return true
		}
	}
	return false
}

// parallel says scenarios run side by side (within each goal):
// CAIRN_TEST_PARALLEL set to a number above 0 (pass the same as go test
// -parallel to bound how many).
var parallel = func() bool {
	n, err := strconv.Atoi(os.Getenv("CAIRN_TEST_PARALLEL"))
	return err == nil && n > 0
}()

func runScenario(t *testing.T, s spec.Scenario) {
	t.Run(testName(s), func(t *testing.T) {
		if parallel && !isExclusive(s) {
			t.Parallel()
		}
		fn, implemented := Impl[s.ID]
		switch {
		case implemented:
			t.Run("self", func(t *testing.T) {
				env := NewEnv(t)
				for _, n := range s.Needs {
					env.Need(n)
				}
				fn(env)
			})
		case s.Bench != "":
			t.Run("self", func(t *testing.T) {
				t.Skipf("performance scenario: run %s with `make bench`", s.Bench)
			})
		case len(s.Children) == 0:
			t.Skipf("pending (%s): not implemented yet", s.Milestone)
		}
		for _, c := range s.Children {
			runScenario(t, c)
		}
	})
}

func testName(s spec.Scenario) string {
	return s.ID + "_" + strings.ReplaceAll(s.Title, " ", "_")
}

// Orphans returns implemented IDs that are not in the catalog.
func Orphans() []string {
	known := map[string]bool{}
	spec.Walk(spec.Catalog, func(s spec.Scenario, _ []spec.Scenario) { known[s.ID] = true })
	var out []string
	for id := range Impl {
		if !known[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Status is a scenario's state for reports.
func Status(s spec.Scenario) string {
	switch {
	case Impl[s.ID] != nil:
		return "implemented"
	case s.Bench != "":
		return "benchmark"
	case len(s.Children) > 0:
		return "group"
	default:
		return "pending"
	}
}
