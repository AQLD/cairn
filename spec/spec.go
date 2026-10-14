// Package spec is Cairn's catalog of end-goal tests: everything the platform
// must handle, as one tree.
//
// The top level is the end goals (what a user or operator can count on). Each
// goal breaks down into scenarios, and scenarios into narrower ones. To grow the
// suite, add a child where it belongs; it shows up in the test run (as pending)
// and in docs/test-plan.md immediately, and turns green once it is implemented
// in package acceptance.
//
// The tree is the single source of truth for:
//   - the acceptance run (go test ./acceptance), which walks it,
//   - the benchmarks (go test -bench . ./bench), named in Scenario.Bench,
//   - the milestone gates in ROADMAP.md, which list scenario IDs.
package spec

// Milestone is when a scenario must pass. A milestone's gate passes only when
// every scenario marked with it (and every earlier one) is green.
type Milestone string

const (
	E0    Milestone = "E0" // prove it can work: gVisor save/resume across servers
	E1    Milestone = "E1" // one server, done right
	E2    Milestone = "E2" // never lose a save
	E3    Milestone = "E3" // agents live in it
	E4    Milestone = "E4" // bulletproof: safe for strangers, measured
	E5    Milestone = "E5" // many servers
	Later Milestone = "later"
)

// Milestones in order.
var Milestones = []Milestone{E0, E1, E2, E3, E4, E5, Later}

// Need is something a scenario requires from its environment. Scenarios whose
// needs are not met are skipped with the reason, never passed.
type Need string

const (
	// Runtime capabilities, as reported by GET /v1/info.
	NeedIsolation      Need = "isolation"
	NeedFullSave       Need = "full_save"
	NeedResourceLimits Need = "resource_limits"
	NeedNetworkControl Need = "network_control"
	NeedPersistent     Need = "persistent_processes"

	// Test environment features.
	NeedInProcess     Need = "in_process"     // the test controls the engine (restart, failpoints, disk access)
	NeedLinuxBench    Need = "linux_bench"    // real droplets: fresh installs, CPU types, kill -9, soak
	NeedObjectStorage Need = "object_storage" // an S3-compatible bucket
	NeedMultiServer   Need = "multi_server"   // more than one engine server
	NeedRealAgent     Need = "real_agent"     // a real coding agent driving the engine
)

// Scenario is one end-goal test or a piece of one.
type Scenario struct {
	ID        string
	Title     string
	Proves    string // what passing guarantees, in one sentence
	Milestone Milestone
	Needs     []Need
	Bench     string // for performance scenarios: the benchmark that measures it
	Children  []Scenario
}

// Walk visits every scenario depth-first with its parent chain.
func Walk(scenarios []Scenario, fn func(s Scenario, parents []Scenario)) {
	var walk func([]Scenario, []Scenario)
	walk = func(list []Scenario, parents []Scenario) {
		for _, s := range list {
			fn(s, parents)
			walk(s.Children, append(append([]Scenario{}, parents...), s))
		}
	}
	walk(scenarios, nil)
}

// Find returns the scenario with id, if any.
func Find(id string) (Scenario, bool) {
	var found Scenario
	ok := false
	Walk(Catalog, func(s Scenario, _ []Scenario) {
		if s.ID == id {
			found, ok = s, true
		}
	})
	return found, ok
}
