package eventlog

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openLog(t *testing.T, path string, opts Options) *Log {
	t.Helper()
	l, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func list(t *testing.T, l *Log, ws string, q Query) []Event {
	t.Helper()
	l.Flush(context.Background())
	evs, err := l.List(context.Background(), ws, q)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestRecordListAndFilter(t *testing.T) {
	l := openLog(t, filepath.Join(t.TempDir(), "events.db"), Options{})
	for _, k := range []string{"exec", "file_write", "file_read", "exec"} {
		l.Record(Event{Workspace: "a", Kind: k, Data: map[string]any{"n": 1}})
	}
	l.Record(Event{Workspace: "b", Kind: "exec"})
	if got := len(list(t, l, "a", Query{})); got != 4 {
		t.Fatalf("a has %d events, want 4", got)
	}
	if got := len(list(t, l, "a", Query{Kind: "file_*"})); got != 2 {
		t.Fatalf("prefix matched %d, want 2", got)
	}
	// "_" in a prefix is literal, not a LIKE wildcard
	l.Record(Event{Workspace: "a", Kind: "fileXwrite"})
	if got := len(list(t, l, "a", Query{Kind: "file_*"})); got != 2 {
		t.Fatalf("prefix with _ matched %d, want 2", got)
	}
	tail := list(t, l, "a", Query{Tail: true, Limit: 2})
	if len(tail) != 2 || tail[0].Kind != "exec" || tail[1].Kind != "fileXwrite" || tail[0].ID >= tail[1].ID {
		t.Fatalf("tail: %+v", tail)
	}
	if n, _ := list(t, l, "a", Query{})[0].Data["n"].(float64); n != 1 {
		t.Fatal("data did not round-trip")
	}
}

func TestBoundPerWorkspace(t *testing.T) {
	l := openLog(t, filepath.Join(t.TempDir(), "events.db"), Options{PerWorkspace: 20})
	for i := 0; i < 500; i++ {
		l.Record(Event{Workspace: "a", Kind: "exec"})
		l.Record(Event{Workspace: "b", Kind: "exec"})
	}
	for _, ws := range []string{"a", "b"} {
		if n := len(list(t, l, ws, Query{Limit: 1000})); n < 20 || n > 22 {
			t.Fatalf("%s kept %d events, want about 20", ws, n)
		}
	}
}

func TestChangedWakesFollowers(t *testing.T) {
	l := openLog(t, filepath.Join(t.TempDir(), "events.db"), Options{})
	last := l.Last()
	ch := l.Changed(last)
	select {
	case <-ch:
		t.Fatal("changed before anything happened")
	default:
	}
	l.Record(Event{Workspace: "a", Kind: "exec"})
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("follower not woken")
	}
	if l.Last() <= last {
		t.Fatal("Last did not move")
	}
}

func TestRecordNeverBlocksAndCloseIsSafe(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "events.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				l.Record(Event{Workspace: "a", Kind: "exec"})
			}
		}()
	}
	time.Sleep(time.Millisecond)
	if err := l.Close(); err != nil { // while recording goes on
		t.Fatal(err)
	}
	wg.Wait()
	l.Record(Event{Workspace: "a", Kind: "exec"}) // after Close: dropped, no panic
	select {
	case <-l.Changed(l.Last()):
	default:
		t.Fatal("a closed log must release followers")
	}
}

func TestDamagedLogIsSetAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.db")
	if err := os.WriteFile(path, []byte("this is not a database, just junk bytes that fill a page "+string(make([]byte, 4096))), 0o600); err != nil {
		t.Fatal(err)
	}
	l := openLog(t, path, Options{})
	l.Record(Event{Workspace: "a", Kind: "exec"})
	if len(list(t, l, "a", Query{})) != 1 {
		t.Fatal("a fresh log should work")
	}
	m, _ := filepath.Glob(path + ".unreadable-*")
	if len(m) == 0 {
		t.Fatal("the damaged log should be kept aside")
	}
}

func TestPurgeEndedWorkspaces(t *testing.T) {
	l := openLog(t, filepath.Join(t.TempDir(), "events.db"), Options{})
	l.Record(Event{Workspace: "gone", Kind: "delete"})
	l.Record(Event{Workspace: "live", Kind: "exec"})
	l.Flush(context.Background())
	now := time.Now()
	if err := l.Ended(context.Background(), "gone", now); err != nil {
		t.Fatal(err)
	}
	if n, _ := l.Purge(context.Background(), now.Add(-time.Hour)); n != 0 {
		t.Fatal("purged inside the window")
	}
	if n, _ := l.Purge(context.Background(), now.Add(time.Hour)); n != 1 {
		t.Fatal("not purged after the window")
	}
	if len(list(t, l, "gone", Query{})) != 0 || len(list(t, l, "live", Query{})) != 1 {
		t.Fatal("purge removed the wrong log")
	}
}
