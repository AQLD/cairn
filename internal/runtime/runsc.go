package runtime

import (
	"context"
	"fmt"
)

// Runsc will drive gVisor's runsc directly (not through Docker), so Cairn
// controls exactly how saves and restores happen.
//
// Not built yet. The E0 spike (see ROADMAP.md) proves save and resume of real
// workloads across two servers with runsc before this driver is written; until
// then every method reports which milestone it belongs to.
type Runsc struct {
	Binary string // path to runsc
}

func NewRunsc(binary string) *Runsc { return &Runsc{Binary: binary} }

func (*Runsc) Name() string    { return "runsc" }
func (*Runsc) Version() string { return "unknown" }

// Capabilities are what runsc will guarantee once the driver lands. They stay
// false until each one is implemented and covered by the acceptance suite.
func (*Runsc) Capabilities() Capabilities { return Capabilities{} }

func notYet(op, milestone string) error {
	return fmt.Errorf("runsc %s: %w yet (planned for %s)", op, ErrUnsupported, milestone)
}

func (*Runsc) Start(context.Context, Spec) error { return notYet("start", "E1") }
func (*Runsc) Exec(context.Context, Spec, ExecRequest) (ExecResult, error) {
	return ExecResult{}, notYet("exec", "E1")
}
func (*Runsc) Stop(context.Context, Spec) error                   { return notYet("stop", "E1") }
func (*Runsc) Checkpoint(context.Context, Spec, string) error     { return notYet("checkpoint", "E1") }
func (*Runsc) Restore(context.Context, Spec, string) error        { return notYet("restore", "E1") }
