package engine

import (
	"bytes"
	"context"
	"time"

	"github.com/AQLD/cairn/internal/runtime"
)

// probeAfterRestore makes a workspace's outside connections, just brought
// back from memory, learn whether their peers still have them (see
// runtime/probe.go), so a program waiting on one its peer dropped during the
// save gets an error rather than waiting forever. In the background: a
// resume never waits for it.
func (e *Engine) probeAfterRestore(id string) {
	prober, ok := e.cfg.Runtime.(runtime.Prober)
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		w, err := e.store.GetWorkspace(ctx, id)
		if err != nil {
			return
		}
		spec, err := specOf(w)
		if err != nil || !spec.Network {
			return
		}
		var out bytes.Buffer
		res, err := e.exec(ctx, id, ExecRequest{Argv: []string{"cat", "/proc/net/tcp"}, Stdout: &out, Timeout: 10 * time.Second})
		if err != nil || res.ExitCode != 0 {
			e.log.Debug("could not list a restored workspace's connections", "workspace", id, "err", err)
			return
		}
		conns := runtime.ParseProcNetTCP(out.String())
		if len(conns) == 0 {
			return
		}
		rs, err := e.runtimeSpec(w)
		if err != nil {
			return
		}
		if err := prober.ProbeConnections(rs, conns); err != nil {
			e.log.Warn("could not probe a restored workspace's connections", "workspace", id, "err", err)
			return
		}
		e.log.Info("probed a restored workspace's outside connections", "workspace", id, "connections", len(conns))
	}()
}
