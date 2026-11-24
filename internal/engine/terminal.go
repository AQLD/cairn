package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/AQLD/cairn/internal/runtime"
)

// Terminals: a person (or an agent) at an interactive shell in a running
// workspace. The shell is a child of the workspace's init like any command,
// so full saves and resumes treat it the same way. What is typed is never
// recorded: the activity log says a terminal opened and closed.

// TTYSize is a terminal's size.
type TTYSize = runtime.TTYSize

// Terminal is an open terminal. Its output goes to the writer OpenTerminal
// was given; Done is closed when its command ends.
type Terminal struct {
	in   *io.PipeWriter
	mu   sync.Mutex
	done chan struct{}
	code int
	err  error
}

// defaultShell starts bash if the workspace has it, else sh, as a login shell.
var defaultShell = []string{"sh", "-c", "command -v bash >/dev/null && exec bash -l; exec sh -l"}

// OpenTerminal starts argv (a login shell if empty) on a terminal of size in
// a workspace. Close it to end the command, as closing a terminal would.
func (e *Engine) OpenTerminal(ctx context.Context, id string, size TTYSize, argv []string, out io.Writer) (*Terminal, error) {
	if !e.cfg.Runtime.Capabilities().Terminal {
		return nil, fmt.Errorf("%w: runtime %s has no terminals", ErrUnsupported, e.cfg.Runtime.Name())
	}
	if size.Rows == 0 || size.Cols == 0 {
		size = TTYSize{Rows: 24, Cols: 80}
	}
	if len(argv) == 0 {
		argv = defaultShell
	}
	if _, err := e.liveWorkspace(ctx, id); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	t := &Terminal{in: pw, done: make(chan struct{})}
	var head bytes.Buffer // the start of the output, to recognise an old init
	tee := &headWriter{w: out, head: &head, max: 512}
	start := time.Now()
	e.note(ctx, id, "terminal_open", fmt.Sprintf("terminal opened (%dx%d)", size.Cols, size.Rows), nil, map[string]any{"rows": size.Rows, "cols": size.Cols})
	go func() {
		defer close(t.done)
		res, err := e.exec(context.WithoutCancel(ctx), id, ExecRequest{Argv: argv, Stdin: pr, Stdout: tee, Stderr: tee, TTY: &size})
		pr.Close()
		if err == nil && res.ExitCode == 1 && time.Since(start) < 5*time.Second && strings.Contains(head.String(), "address already in use") {
			// a cairn-init from before terminals took the request for itself
			err = fmt.Errorf("%w: this workspace's init predates terminals (make a new workspace, or copy a save into one)", ErrUnsupported)
		}
		t.code, t.err = res.ExitCode, err
		e.note(context.WithoutCancel(ctx), id, "terminal_close", fmt.Sprintf("terminal closed after %s", seconds(time.Since(start))), err,
			map[string]any{"exit_code": res.ExitCode, "duration_ms": time.Since(start).Milliseconds()})
	}()
	return t, nil
}

func (t *Terminal) frame(kind byte, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	hdr := make([]byte, 5)
	hdr[0] = kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := t.in.Write(append(hdr, payload...)); err != nil {
		return io.ErrClosedPipe
	}
	return nil
}

// Write types p into the terminal.
func (t *Terminal) Write(p []byte) (int, error) {
	if err := t.frame(runtime.TerminalData, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize changes the terminal's size.
func (t *Terminal) Resize(size TTYSize) error {
	p := make([]byte, 4)
	binary.BigEndian.PutUint16(p, size.Rows)
	binary.BigEndian.PutUint16(p[2:], size.Cols)
	return t.frame(runtime.TerminalResize, p)
}

// Close ends the terminal's command.
func (t *Terminal) Close() error { return t.in.Close() }

// Done is closed when the command has ended.
func (t *Terminal) Done() <-chan struct{} { return t.done }

// Result is the command's exit code, once Done.
func (t *Terminal) Result() (int, error) { return t.code, t.err }

// headWriter passes writes on and keeps their first max bytes.
type headWriter struct {
	w    io.Writer
	head *bytes.Buffer
	max  int
	mu   sync.Mutex
}

func (h *headWriter) Write(p []byte) (int, error) {
	h.mu.Lock()
	if room := h.max - h.head.Len(); room > 0 {
		h.head.Write(p[:min(room, len(p))])
	}
	h.mu.Unlock()
	return h.w.Write(p)
}
