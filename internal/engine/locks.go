package engine

import (
	"context"
	"fmt"
	goruntime "runtime"
	"sync"
	"time"
)

// DefaultExclusiveWait is how long a full save or a resume waits for running
// commands to finish before answering conflict.
const DefaultExclusiveWait = 10 * time.Second

// wsLock orders operations on one workspace. Commands, file access and quick
// saves share it; full saves, resumes and deletes need it alone. It also knows
// the running commands, so a delete can stop them instead of waiting.
type wsLock struct {
	sync.RWMutex

	mu       sync.Mutex
	next     int
	commands map[int]context.CancelFunc
}

// command registers a running command; the returned function unregisters it.
// A workspace runs at most max commands at once: each costs a runsc process on
// the host, so an unbounded burst would hurt the host, not just the workspace.
func (l *wsLock) command(cancel context.CancelFunc, max int) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.commands) >= max {
		return nil, fmt.Errorf("%w: %d commands are already running in this workspace", ErrBusy, max)
	}
	if l.commands == nil {
		l.commands = map[int]context.CancelFunc{}
	}
	l.next++
	n := l.next
	l.commands[n] = cancel
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.commands, n)
	}, nil
}

func (l *wsLock) cancelCommands() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, cancel := range l.commands {
		cancel()
	}
}

// lockAlone takes the lock exclusively, waiting at most wait for running
// commands. It never queues as a pending writer, which would stall every new
// command behind a long-running one.
func (l *wsLock) lockAlone(ctx context.Context, wait time.Duration, what string) error {
	deadline := time.Now().Add(wait)
	for !l.TryLock() {
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: commands are still running in the workspace; %s needs it idle (wait for them or cancel them)", ErrConflict, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return nil
}

func (e *Engine) lock(id string) *wsLock {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.locks[id]
	if !ok {
		l = &wsLock{}
		e.locks[id] = l
	}
	return l
}

// DefaultMaxCommands is how many commands one workspace may run at once.
const DefaultMaxCommands = 64

// DefaultMaxOps is how many heavy operations (create, save, resume, copy,
// template import) run at once; more wait their turn. They are disk- and
// CPU-bound, so running dozens at once makes every one of them slower.
func DefaultMaxOps() int { return max(4, 2*goruntime.NumCPU()) }

// heavy waits for a slot for a heavy operation (or for ctx to end) and returns
// the function that frees it.
func (e *Engine) heavy(ctx context.Context) (func(), error) {
	select {
	case e.ops <- struct{}{}:
		return func() { <-e.ops }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
