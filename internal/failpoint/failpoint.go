// Package failpoint lets tests make the engine fail at a named point, to prove
// it recovers. Production code calls Check at each point; with no Set, or no
// failpoint enabled, Check is free.
package failpoint

import (
	"errors"
	"sync"
)

// ErrCrash simulates the process dying at this point: the engine skips all
// cleanup, exactly as if it had been killed, and the test reopens it.
var ErrCrash = errors.New("failpoint: simulated crash")

// Names of the points the engine checks. Each is covered by a kill test.
const (
	SaveBeforeCommit  = "save/before-commit"  // save files written, not yet published
	SaveAfterCommit   = "save/after-commit"   // save published, state not yet updated
	ResumeBeforeSwap  = "resume/before-swap"  // new filesystem extracted, not yet swapped in
	ResumeAfterSwap   = "resume/after-swap"   // filesystem swapped, workspace not yet running
	CopyBeforeRunning = "copy/before-running" // copy's filesystem extracted, not yet running
	CreateBeforeReady = "create/before-ready" // workspace recorded, not yet running
	PurgeAfterRemove  = "purge/after-remove"  // save folder removed, record not yet deleted
)

type Set struct {
	mu sync.Mutex
	m  map[string]error
}

func New() *Set { return &Set{m: map[string]error{}} }

// Enable makes Check(name) return err until Disable.
func (s *Set) Enable(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = err
}

func (s *Set) Disable(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
}

// Check returns the error enabled for name, if any. Safe on a nil Set.
func (s *Set) Check(name string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[name]
}

// Crashed reports whether err is a simulated crash, in which case callers must
// skip cleanup.
func Crashed(err error) bool { return errors.Is(err, ErrCrash) }
