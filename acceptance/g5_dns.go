package acceptance

import (
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"

	"github.com/AQLD/cairn/client"
)

// hostNameservers is what the engine should give a workspace that names no
// resolver: the host's own (systemd-resolved's upstream list where it runs),
// IPv4, never loopback; public ones when there are none.
func hostNameservers() []string {
	for _, p := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if ns := nameservers(string(data)); len(ns) > 0 {
			return ns
		}
	}
	return []string{"1.1.1.1", "8.8.8.8"}
}

func nameservers(resolvConf string) []string {
	var out []string
	for _, line := range strings.Split(resolvConf, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() && !a.IsLoopback() {
				out = append(out, f[1])
			}
		}
	}
	return out
}

// restartWith sets ws's /etc/resolv.conf, then starts its sandbox again (a
// quick save and resume), as after an engine upgrade or a move.
func restartWith(e *Env, ws, resolvConf string) {
	e.T.Helper()
	e.Must(e.C.WriteFile(e.Ctx, ws, "etc/resolv.conf", []byte(resolvConf)))
	sv, err := e.C.Save(e.Ctx, ws, client.QuickSave)
	e.Must(err)
	_, err = e.C.Resume(e.Ctx, sv.ID)
	e.Must(err)
}

func init() {
	register("G5.10.1", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Network: true})
		e.Must(err)
		restartWith(e, ws.ID, "") // what images exported from Docker carry
		got := nameservers(e.Read(ws.ID, "etc/resolv.conf"))
		if want := hostNameservers(); !slices.Equal(got, want) {
			e.T.Fatalf("workspace resolvers %v, want the host's %v", got, want)
		}
		// a name the host resolves resolves inside (skipped where the host
		// itself is offline)
		if _, err := net.DefaultResolver.LookupHost(e.Ctx, "pypi.org"); err != nil {
			e.T.Logf("the host cannot resolve pypi.org (%v): resolution inside not checked", err)
			return
		}
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"python3", "-c",
			"import socket; print(socket.gethostbyname('pypi.org'))"}, TimeoutMS: 30_000})
		e.Must(err)
		if res.ExitCode != 0 {
			e.T.Fatalf("pypi.org does not resolve in the workspace though the host resolves it: %s%s", res.Stdout, res.Stderr)
		}
	})

	register("G5.10.2", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Network: true})
		e.Must(err)
		restartWith(e, ws.ID, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n") // what engines up to v0.3.0 wrote
		got := nameservers(e.Read(ws.ID, "etc/resolv.conf"))
		if want := hostNameservers(); !slices.Equal(got, want) {
			e.T.Fatalf("workspace resolvers after a start %v, want the host's %v", got, want)
		}
	})

	register("G5.10.3", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Network: true})
		e.Must(err)
		const mine = "nameserver 9.9.9.9\noptions ndots:1\n"
		restartWith(e, ws.ID, mine)
		Equal(e, "resolv.conf the workspace wrote", e.Read(ws.ID, "etc/resolv.conf"), mine)
	})
}
