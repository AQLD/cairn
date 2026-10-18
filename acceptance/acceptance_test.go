package acceptance

import (
	"os"
	"testing"

	"github.com/AQLD/cairn/spec"
)

func TestMain(m *testing.M) {
	code := m.Run()
	removeTestInit()
	os.Exit(code)
}

// TestEndGoals runs every end-goal scenario in the catalog.
//
//	go test ./acceptance                         # host runtime, in-process
//	go test ./acceptance -run 'TestEndGoals/G6'  # one goal
//	CAIRN_CYCLES=10000 go test ./acceptance -run 'G6.2' -timeout 0   # E1 loop gate
func TestEndGoals(t *testing.T) {
	Run(t)
}

// TestCatalogIsConsistent keeps the catalog and the implementations in step.
func TestCatalogIsConsistent(t *testing.T) {
	if o := Orphans(); len(o) > 0 {
		t.Fatalf("implemented scenarios missing from the catalog: %v", o)
	}
	seen := map[string]bool{}
	spec.Walk(spec.Catalog, func(s spec.Scenario, parents []spec.Scenario) {
		if seen[s.ID] {
			t.Errorf("duplicate scenario id %s", s.ID)
		}
		seen[s.ID] = true
		if s.Title == "" || s.Milestone == "" {
			t.Errorf("%s: title and milestone are required", s.ID)
		}
		if len(parents) > 0 {
			p := parents[len(parents)-1].ID
			if len(s.ID) <= len(p) || s.ID[:len(p)+1] != p+"." {
				t.Errorf("%s: child id must extend its parent %s", s.ID, p)
			}
		}
	})
}

// TestJourneysAreConsistent keeps agent journeys honest: a step relies only on
// capability scenarios that exist, and is never promised before them.
func TestJourneysAreConsistent(t *testing.T) {
	journey := map[string]bool{} // every ID inside an agent journey
	for _, g := range spec.Catalog {
		if g.For != "" {
			spec.Walk([]spec.Scenario{g}, func(s spec.Scenario, _ []spec.Scenario) { journey[s.ID] = true })
		}
	}
	spec.Walk(spec.Catalog, func(s spec.Scenario, parents []spec.Scenario) {
		if s.For != "" && len(parents) > 0 {
			t.Errorf("%s: only a top-level goal can be an agent journey", s.ID)
		}
		if len(s.Uses) > 0 && !journey[s.ID] {
			t.Errorf("%s: only agent journey steps list the scenarios they use", s.ID)
		}
		for _, id := range s.Uses {
			u, ok := spec.Find(id)
			switch {
			case !ok:
				t.Errorf("%s uses unknown scenario %s", s.ID, id)
			case journey[id]:
				t.Errorf("%s uses %s, another journey's step: use the capability scenarios instead", s.ID, id)
			case spec.Before(s.Milestone, u.Milestone):
				t.Errorf("%s (%s) is promised before %s (%s), which it relies on", s.ID, s.Milestone, id, u.Milestone)
			}
		}
	})
}
