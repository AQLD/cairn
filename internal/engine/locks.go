package engine

import (
	"context"
	"fmt"
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
func (l *wsLock) command(cancel context.CancelFunc) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
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
	}
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
