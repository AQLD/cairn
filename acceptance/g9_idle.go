package acceptance

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/AQLD/cairn/client"
)

// G9.8: a paused or fully saved workspace costs only its storage: no sandbox
// processes, no memory, no cgroup on the host. Its neighbour keeps running.

func init() {
	register("G9.8", func(e *Env) {
		idle := e.Workspace("idle")
		busy := e.Workspace("busy")
		background(e, idle.ID, counterProgram)
		before := running(e, idle.ID)
		if sandboxProcs(idle.ID) == 0 {
			e.T.Skip("cannot see sandbox processes from here (needs the runsc runtime on this host)")
		}

		_, err := e.C.PauseWorkspace(e.Ctx, idle.ID)
		e.Must(err)
		Equal(e, "sandbox processes of the paused workspace", sandboxProcs(idle.ID), 0)
		Equal(e, "its cgroup is gone", hasCgroup(idle.ID), false)
		if sandboxProcs(busy.ID) == 0 {
			e.T.Fatal("pausing one workspace stopped its neighbour")
		}
		Equal(e, "neighbour answers", e.Sh(busy.ID, "echo ok"), "ok\n")

		// it costs nothing until used, then carries on where it was
		Equal(e, "wakes on use", e.Sh(idle.ID, "echo awake"), "awake\n")
		Equal(e, "same process after waking", running(e, idle.ID).PID, before.PID)

		// a full save leaves it stopped, and free too
		_, err = e.C.Save(e.Ctx, idle.ID, client.FullSave)
		e.Must(err)
		Equal(e, "sandbox processes after a full save", sandboxProcs(idle.ID), 0)
		Equal(e, "cgroup after a full save", hasCgroup(idle.ID), false)
	})
}

// sandboxProcs counts the host processes of a workspace's sandbox.
func sandboxProcs(ws string) int {
	n := 0
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if strings.Contains(args[0], "runsc") && strings.Contains(string(b), "cairn-"+ws) {
			n++
		}
	}
	return n
}

// hasCgroup reports whether the workspace's sandbox still has a cgroup.
func hasCgroup(ws string) bool {
	_, err := os.Stat(filepath.Join("/sys/fs/cgroup/cairn", "cairn-"+ws))
	return err == nil
}
