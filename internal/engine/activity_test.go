package engine

import (
	"context"
	"testing"

	"github.com/AQLD/cairn/internal/runtime"
)

// Requests for workspaces that do not exist must not leave logs behind: the
// API cannot show them (it answers not_found), so they would only pile up.
func TestNoLogForUnknownWorkspaces(t *testing.T) {
	ctx := context.Background()
	e, err := Open(ctx, Config{DataDir: t.TempDir(), Runtime: runtime.NewHost(), NoDiskImages: true})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	const ghost = "ws_0000000000000000"
	if _, err := e.Exec(ctx, ghost, ExecRequest{Argv: []string{"true"}}); err == nil {
		t.Fatal("exec in a missing workspace succeeded")
	}
	if _, err := e.Save(ctx, ghost, "", ""); err == nil {
		t.Fatal("save of a missing workspace succeeded")
	}
	e.FlushEvents(ctx)
	evs, err := e.events.List(ctx, ghost, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("a missing workspace got %d events: %+v", len(evs), evs)
	}
}
