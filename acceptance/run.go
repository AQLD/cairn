package acceptance

import (
	"sort"
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

func runScenario(t *testing.T, s spec.Scenario) {
	t.Run(testName(s), func(t *testing.T) {
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
