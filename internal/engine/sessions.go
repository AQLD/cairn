package engine

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sessions: an interpreter (Python or a shell) kept running in a workspace, so
// variables, imports, loaded data and the working folder carry over between
// calls, the way a notebook kernel does. A session is an ordinary process in
// the workspace, started and driven through Exec: it survives full saves and
// pauses like any other process, and needs no state of its own in the engine.
// Its folder in the workspace (/run/cairn/sessions/ID) holds the driver and
// the FIFOs a run talks to it through (see session/python.py).

//go:embed session/python.py
var pythonDriver string

//go:embed session/shell.sh
var shellDriver string

const sessionsDir = "/run/cairn/sessions"

// Session is an interpreter kept running in a workspace.
type Session struct {
	ID       string `json:"id"`
	Language string `json:"language"` // "python" or "shell"
	PID      int    `json:"pid"`      // inside the workspace
	Cells    int    `json:"cells"`    // cells run so far
}

// SessionRun is one cell to run in a session.
type SessionRun struct {
	Code           string
	Stdout, Stderr io.Writer
	// Timeout interrupts the cell (KeyboardInterrupt in Python, SIGINT to the
	// shell's commands); the session itself carries on.
	Timeout time.Duration
}

// SessionResult is how a cell ended. Ended means the session's process died
// while running it (it exited, or was stopped for memory); start a new one.
type SessionResult struct {
	ExitCode int  `json:"exit_code"`
	TimedOut bool `json:"timed_out"`
	Ended    bool `json:"ended,omitempty"`
}

var sessionIDPattern = regexp.MustCompile(`^ses_[0-9a-f]{16}$`)

// alive: the session's process is running its driver. A pid alone is not
// enough: after a resume from a quick save the folder is back but the process
// is not, and its pid may now belong to anything.
const aliveFn = `alive() { [ -n "$pid" ] && grep -qa "$d/driver" "/proc/$pid/cmdline" 2>/dev/null; }
`

// Each script prints a status line ("CAIRN-SESSION ...") before anything else
// on stdout. Arguments arrive as $1..., never inside the script.
const createSessionScript = `d=` + sessionsDir + `/$1 lang=$2
case $lang in
python) interp=$(command -v python3) || { echo "CAIRN-SESSION missing python3"; exit 0; } ;;
*) interp=$(command -v bash || command -v sh) ;;
esac
mkdir -p "$d" && chmod 700 "$d" && cat > "$d/driver" && mkfifo "$d/go" "$d/done" && echo "$lang" > "$d/language" && echo 0 > "$d/seq" || exit 1
# sh starts background jobs with SIGINT ignored, and a shell cannot undo that,
# so timeouts could not interrupt a shell session's commands: restore it
# (coreutils env). Its own session and process group, so delete can stop
# everything it started.
r=; env --default-signal=INT true 2>/dev/null && r="env --default-signal=INT"
s=; command -v setsid >/dev/null 2>&1 && s=setsid
nohup $r $s "$interp" "$d/driver" "$d" "$interp" </dev/null >/dev/null 2>"$d/driver.log" &
echo $! > "$d/pid"
echo "CAIRN-SESSION ok $!"
`

const runSessionScript = aliveFn + `d=` + sessionsDir + `/$1 t=$2
pid=$(cat "$d/pid" 2>/dev/null)
alive || { echo "CAIRN-SESSION gone"; exit 0; }
[ -e "$d/running" ] && { echo "CAIRN-SESSION busy"; exit 0; }
n=$(( $(cat "$d/seq") + 1 )); echo "$n" > "$d/seq"
cat > "$d/cell"
target=$pid; [ "$(cat "$d/language")" = shell ] && target=-$pid
# the watchdog interrupts the cell at its deadline and reports the session's
# death, so a run never waits on a dead session
(
  i=0
  while alive; do
    sleep 0.2; i=$((i + 200))
    if [ "$i" -ge "$t" ] && [ ! -e "$d/interrupted.$n" ]; then
      touch "$d/interrupted.$n"; kill -s INT -- "$target" 2>/dev/null
    fi
  done
  echo "$n gone" > "$d/done"
) </dev/null >/dev/null 2>&1 &
w=$!
exec 3<>"$d/done" # read-write: never an end of file before the watchdog speaks
echo "$n" > "$d/go"
while read -r m st <&3; do [ "$m" = "$n" ] && break; done
kill "$w" 2>/dev/null
i=0; [ -e "$d/interrupted.$n" ] && i=1
echo "CAIRN-SESSION ok $st $i"
cat "$d/out.$n" 2>/dev/null; cat "$d/err.$n" >&2 2>/dev/null
rm -f "$d/out.$n" "$d/err.$n" "$d/interrupted.$n"
`

const listSessionsScript = aliveFn + `echo "CAIRN-SESSION ok"
for d in ` + sessionsDir + `/ses_*; do
  [ -d "$d" ] || continue
  pid=$(cat "$d/pid" 2>/dev/null)
  alive || continue
  echo "${d##*/} $(cat "$d/language") $pid $(cat "$d/seq")"
done
`

const deleteSessionScript = aliveFn + `d=` + sessionsDir + `/$1
[ -d "$d" ] || { echo "CAIRN-SESSION gone"; exit 0; }
pid=$(cat "$d/pid" 2>/dev/null)
if alive; then kill -s KILL -- "-$pid" 2>/dev/null || kill -s KILL "$pid" 2>/dev/null; fi
rm -rf "$d"
echo "CAIRN-SESSION ok"
`

// sessionScript runs one of the scripts above and returns its status words
// and the rest of its stdout (which goes to stdout instead when it is set).
func (e *Engine) sessionScript(ctx context.Context, id, script string, stdin io.Reader, stdout, stderr io.Writer, timeout time.Duration, args ...string) ([]string, []byte, error) {
	if !e.cfg.Runtime.Capabilities().PersistentProcesses {
		return nil, nil, fmt.Errorf("sessions: %w (runtime %s does not keep processes between commands)", ErrUnsupported, e.cfg.Runtime.Name())
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	var rest bytes.Buffer
	if stdout == nil {
		stdout = &rest
	}
	if stderr == nil {
		stderr = io.Discard
	}
	status := &statusLine{rest: stdout}
	var errOut bytes.Buffer
	res, err := e.Exec(ctx, id, ExecRequest{
		Argv:   append([]string{"sh", "-c", script, "cairn-session"}, args...),
		Stdin:  stdin,
		Stdout: status,
		Stderr: io.MultiWriter(stderr, &capWriter{buf: &errOut, max: 4 << 10}),
		// the run script interrupts at its own deadline; this is the backstop
		Timeout: timeout + 30*time.Second,
	})
	if err != nil {
		return nil, nil, err
	}
	words := strings.Fields(status.line.String())
	if len(words) < 2 || words[0] != "CAIRN-SESSION" {
		if res.TimedOut {
			return nil, nil, fmt.Errorf("%w: the session did not answer within %v", ErrConflict, timeout+30*time.Second)
		}
		return nil, nil, fmt.Errorf("session command failed (exit %d): %s", res.ExitCode, strings.TrimSpace(errOut.String()))
	}
	return words[1:], rest.Bytes(), nil
}

// CreateSession starts a session in a running workspace. language is
// "python" (needs python3 in the workspace) or "shell" (bash, else sh).
func (e *Engine) CreateSession(ctx context.Context, id, language string) (*Session, error) {
	driver := map[string]string{"python": pythonDriver, "shell": shellDriver}[language]
	if driver == "" {
		return nil, fmt.Errorf("%w: language must be python or shell", ErrInvalid)
	}
	sid := "ses_" + strings.TrimPrefix(newID("x"), "x_")
	words, _, err := e.sessionScript(ctx, id, createSessionScript, strings.NewReader(driver), nil, nil, time.Minute, sid, language)
	if err != nil {
		return nil, err
	}
	switch {
	case words[0] == "missing":
		return nil, fmt.Errorf("%w: a %s session needs %s in the workspace", ErrInvalid, language, strings.Join(words[1:], " "))
	case words[0] != "ok" || len(words) < 2:
		return nil, fmt.Errorf("session did not start: %v", words)
	}
	pid, _ := strconv.Atoi(words[1])
	return &Session{ID: sid, Language: language, PID: pid}, nil
}

// ListSessions lists the sessions whose process is alive.
func (e *Engine) ListSessions(ctx context.Context, id string) ([]Session, error) {
	_, out, err := e.sessionScript(ctx, id, listSessionsScript, nil, nil, nil, time.Minute)
	if err != nil {
		return nil, err
	}
	out2 := []Session{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 4 {
			continue
		}
		pid, _ := strconv.Atoi(f[2])
		cells, _ := strconv.Atoi(f[3])
		out2 = append(out2, Session{ID: f[0], Language: f[1], PID: pid, Cells: cells})
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].ID < out2[j].ID })
	return out2, nil
}

// RunInSession runs one cell. One cell runs at a time per session: a second
// call while one runs is a conflict.
func (e *Engine) RunInSession(ctx context.Context, id, sid string, run SessionRun) (SessionResult, error) {
	if !sessionIDPattern.MatchString(sid) {
		return SessionResult{}, fmt.Errorf("%w: session %s", ErrNotFound, sid)
	}
	key := id + "/" + sid
	if _, busy := e.running.LoadOrStore(key, struct{}{}); busy {
		return SessionResult{}, fmt.Errorf("%w: session %s is running a cell; wait for it or delete the session", ErrConflict, sid)
	}
	defer e.running.Delete(key)
	if run.Timeout <= 0 {
		run.Timeout = 10 * time.Minute
	}
	words, _, err := e.sessionScript(ctx, id, runSessionScript, strings.NewReader(run.Code), run.Stdout, run.Stderr, run.Timeout,
		sid, strconv.FormatInt(run.Timeout.Milliseconds(), 10))
	if err != nil {
		return SessionResult{}, err
	}
	switch {
	case words[0] == "gone":
		return SessionResult{}, fmt.Errorf("%w: session %s (it may have ended, or the workspace was resumed from a save made without it)", ErrNotFound, sid)
	case words[0] == "busy":
		return SessionResult{}, fmt.Errorf("%w: session %s is still running an earlier cell; wait for it or delete the session", ErrConflict, sid)
	case words[0] != "ok" || len(words) < 3:
		return SessionResult{}, fmt.Errorf("session run: unexpected status %v", words)
	}
	if words[1] == "gone" {
		return SessionResult{ExitCode: -1, TimedOut: words[2] == "1", Ended: true}, nil
	}
	code, err := strconv.Atoi(words[1])
	if err != nil {
		return SessionResult{}, fmt.Errorf("session run: unexpected status %v", words)
	}
	return SessionResult{ExitCode: code, TimedOut: words[2] == "1"}, nil
}

// DeleteSession stops a session and everything it started.
func (e *Engine) DeleteSession(ctx context.Context, id, sid string) error {
	if !sessionIDPattern.MatchString(sid) {
		return fmt.Errorf("%w: session %s", ErrNotFound, sid)
	}
	words, _, err := e.sessionScript(ctx, id, deleteSessionScript, nil, nil, nil, time.Minute, sid)
	if err != nil {
		return err
	}
	if words[0] == "gone" {
		return fmt.Errorf("%w: session %s", ErrNotFound, sid)
	}
	return nil
}

// statusLine passes everything after the first line of output to rest.
type statusLine struct {
	line bytes.Buffer
	done bool
	rest io.Writer
}

func (s *statusLine) Write(p []byte) (int, error) {
	n := len(p)
	if !s.done {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if s.line.Len() < 1024 {
				s.line.Write(p)
			}
			return n, nil
		}
		s.line.Write(p[:i])
		s.done, p = true, p[i+1:]
	}
	if len(p) > 0 {
		if _, err := s.rest.Write(p); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// capWriter keeps the first max bytes written.
type capWriter struct {
	buf *bytes.Buffer
	max int
}

func (c *capWriter) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
