package acceptance

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AQLD/cairn/client"
)

// G1.8: a workspace resumed long after it was saved. gVisor computes the
// sandbox's clocks from the host's on restore, so the mechanism does not
// depend on the pause's length: a pause of seconds (CAIRN_PAUSE to choose)
// stands in for days.

func pauseFor() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("CAIRN_PAUSE")); err == nil && d > 0 {
		return d
	}
	if testing.Short() {
		return 8 * time.Second
	}
	return 30 * time.Second
}

// clockLog starts a program that writes "wall monotonic" every 200 ms.
const clockLog = `nohup python3 -c "
import time
while True:
    with open('/tmp/clock.log', 'a') as f:
        f.write('%.3f %.3f\n' % (time.time(), time.monotonic()))
    time.sleep(0.2)
" >/dev/null 2>&1 &`

type tick struct{ wall, mono float64 }

func readTicks(e *Env, ws string) []tick {
	e.T.Helper()
	var out []tick
	for _, line := range strings.Split(strings.TrimSpace(e.Read(ws, "tmp/clock.log")), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue // a line cut by the save
		}
		w, err1 := strconv.ParseFloat(f[0], 64)
		m, err2 := strconv.ParseFloat(f[1], 64)
		if err1 == nil && err2 == nil {
			out = append(out, tick{w, m})
		}
	}
	return out
}

// pauseAndResume full-saves the workspace, waits, resumes it, and returns how
// long it was saved by the host's clock.
func pauseAndResume(e *Env, ws string) (saved, resumed time.Time) {
	e.T.Helper()
	sv, err := e.C.Save(e.Ctx, ws, client.FullSave)
	e.Must(err)
	saved = time.Now()
	time.Sleep(pauseFor())
	_, err = e.C.Resume(e.Ctx, sv.ID)
	e.Must(err)
	return saved, time.Now()
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func init() {
	register("G1.8.1", func(e *Env) {
		ws := e.Workspace("days-clock")
		e.Sh(ws.ID, clockLog)
		time.Sleep(time.Second)
		saved, resumed := pauseAndResume(e, ws.ID)
		pause := resumed.Sub(saved).Seconds()

		inside, err := strconv.ParseFloat(strings.TrimSpace(e.Sh(ws.ID, "date +%s.%N")), 64)
		e.Must(err)
		if d := inside - unix(time.Now()); d < -2 || d > 2 {
			e.T.Fatalf("the workspace's clock is %.1f s off the real time after a %.0f s pause", d, pause)
		}

		time.Sleep(time.Second) // let the logger write a few lines after the resume
		ticks := readTicks(e, ws.ID)
		gap := -1
		for i := 1; i < len(ticks); i++ {
			if ticks[i].mono < ticks[i-1].mono {
				e.T.Fatalf("monotonic time went back: %.3f then %.3f", ticks[i-1].mono, ticks[i].mono)
			}
			if ticks[i].wall-ticks[i-1].wall > pause/2 {
				gap = i
			}
		}
		if gap < 0 {
			e.T.Fatalf("no pause visible in the clock log: the logger did not survive the save (%d lines)", len(ticks))
		}
		before, after := ticks[gap-1], ticks[gap]
		wallJump, monoJump := after.wall-before.wall, after.mono-before.mono
		if wallJump < pause-2 || wallJump > pause+3 {
			e.T.Fatalf("wall clock jumped %.1f s over a %.1f s pause", wallJump, pause)
		}
		if monoJump < pause-2 || monoJump > pause+3 {
			e.T.Fatalf("monotonic clock moved %.1f s over a %.1f s pause, want it to count the pause", monoJump, pause)
		}
		e.T.Logf("paused %.1f s: wall clock +%.1f s, monotonic +%.1f s", pause, wallJump, monoJump)
	})

	register("G1.8.2", func(e *Env) {
		ws := e.Workspace("days-timers")
		short := pauseFor() / 2 // falls due in the middle of the pause
		e.Sh(ws.ID, fmt.Sprintf(`nohup sh -c 'sleep %d; date +%%s.%%N > /tmp/timer.fired' >/dev/null 2>&1 &`, int(short.Seconds())))
		e.Sh(ws.ID, clockLog) // periodic work
		time.Sleep(time.Second)
		_, resumed := pauseAndResume(e, ws.ID)

		var fired string
		for i := 0; i < 50; i++ {
			if b, err := e.C.ReadFile(e.Ctx, ws.ID, "tmp/timer.fired"); err == nil && len(b) > 0 {
				fired = strings.TrimSpace(string(b))
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if fired == "" {
			e.T.Fatalf("a %v timer due during the pause had not fired 5 s after resume", short)
		}
		at, err := strconv.ParseFloat(fired, 64)
		e.Must(err)
		if late := at - unix(resumed); late > 3 {
			e.T.Fatalf("the timer fired %.1f s after resume, want promptly", late)
		}

		before := len(readTicks(e, ws.ID))
		time.Sleep(time.Second)
		if after := len(readTicks(e, ws.ID)); after < before+3 {
			e.T.Fatalf("periodic work did not carry on after resume: %d lines, then %d", before, after)
		}
	})
}
