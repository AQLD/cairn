package acceptance

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/runtime"
)

// G5.4: outbound traffic follows an allow-list.
//
// The scenarios build a small internet on the host: a network namespace with
// two addresses serving HTTP, reachable from workspaces through the host's
// NAT like any outside server, and a DNS server in this process that the
// engine's forwarder asks for allowed names (allowed.test and x.ok.test point
// at the first address, other.test at the second).

type egressNet struct {
	allowedIP, otherIP string
	dnsAddr            string
}

func newEgressNet(e *Env) *egressNet {
	e.T.Helper()
	h := fnv.New32a()
	h.Write([]byte(e.T.Name()))
	n := h.Sum32() % 200
	ns, hostIf, nsIf := fmt.Sprintf("cairn-t%d", n), fmt.Sprintf("ct%dh", n), fmt.Sprintf("ct%dn", n)
	base := fmt.Sprintf("10.251.%d", n)
	hostIP, allowed, other := base+".1", base+".2", base+".3"
	sh := func(args ...string) {
		e.T.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			e.T.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	_ = exec.Command("ip", "netns", "del", ns).Run()
	_ = exec.Command("ip", "link", "del", hostIf).Run()
	sh("ip", "netns", "add", ns)
	sh("ip", "link", "add", hostIf, "type", "veth", "peer", "name", nsIf)
	sh("ip", "link", "set", nsIf, "netns", ns)
	sh("ip", "addr", "add", hostIP+"/29", "dev", hostIf)
	sh("ip", "link", "set", hostIf, "up")
	sh("ip", "-n", ns, "addr", "add", allowed+"/29", "dev", nsIf)
	sh("ip", "-n", ns, "addr", "add", other+"/29", "dev", nsIf)
	sh("ip", "-n", ns, "link", "set", nsIf, "up")
	sh("ip", "-n", ns, "link", "set", "lo", "up")
	srv := exec.Command("ip", "netns", "exec", ns, "python3", "-m", "http.server", "8080", "--bind", "0.0.0.0")
	e.Must(srv.Start())
	e.T.Cleanup(func() {
		_ = srv.Process.Kill()
		_ = srv.Wait()
		_ = exec.Command("ip", "link", "del", hostIf).Run()
		_ = exec.Command("ip", "netns", "del", ns).Run()
	})

	// the upstream DNS the engine's forwarder asks
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	e.Must(err)
	e.T.Cleanup(func() { pc.Close() })
	go serveTestDNS(pc, map[string]string{"allowed.test": allowed, "x.ok.test": allowed, "other.test": other})
	rs, ok := e.rt.(*runtime.Runsc)
	if !ok {
		e.T.Skip("needs the runsc runtime")
	}
	rs.Upstream = []string{pc.LocalAddr().String()}
	e.T.Cleanup(func() { rs.Upstream = nil })

	// the server is up before anyone asks
	deadline := time.Now().Add(10 * time.Second)
	for exec.Command("ip", "netns", "exec", ns, "python3", "-c", "import socket; socket.create_connection(('"+allowed+"', 8080), 1)").Run() != nil {
		if time.Now().After(deadline) {
			e.T.Fatal("test web server did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return &egressNet{allowedIP: allowed, otherIP: other, dnsAddr: pc.LocalAddr().String()}
}

// serveTestDNS answers A queries from names; anything else gets no answers.
func serveTestDNS(pc net.PacketConn, names map[string]string) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte{}, buf[:n]...)
		if len(q) < 12 {
			continue
		}
		// the question's name
		var labels []string
		off := 12
		for off < len(q) && q[off] != 0 {
			l := int(q[off])
			if off+1+l > len(q) {
				break
			}
			labels = append(labels, strings.ToLower(string(q[off+1:off+1+l])))
			off += 1 + l
		}
		qend := off + 5
		if qend > len(q) {
			continue
		}
		qtype := binary.BigEndian.Uint16(q[off+1 : off+3])
		resp := append([]byte{}, q[:qend]...)
		resp[2] |= 0x80
		resp[3] |= 0x80
		if ip := net.ParseIP(names[strings.Join(labels, ".")]).To4(); ip != nil && qtype == 1 {
			binary.BigEndian.PutUint16(resp[6:8], 1)
			resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 1, 0x2c, 0, 4)
			resp = append(resp, ip...)
		} else if names[strings.Join(labels, ".")] == "" {
			resp[3] = resp[3]&0xf0 | 3 // NXDOMAIN
		}
		_, _ = pc.WriteTo(resp, from)
	}
}

// fetch runs a Python fetch inside the workspace and returns its output and
// whether it succeeded.
func fetch(e *Env, ws, target string) (string, bool) {
	e.T.Helper()
	res, err := e.C.Exec(e.Ctx, ws, client.Exec{Command: []string{"python3", "-c",
		"import urllib.request,sys\ntry:\n print(urllib.request.urlopen('" + target + "', timeout=4).status)\nexcept Exception as x:\n print(type(x).__name__, x); sys.exit(1)"},
		TimeoutMS: 20_000})
	e.Must(err)
	return strings.TrimSpace(res.Stdout + res.Stderr), res.ExitCode == 0
}

func resolves(e *Env, ws, name string) bool {
	e.T.Helper()
	res, err := e.C.Exec(e.Ctx, ws, client.Exec{Command: []string{"python3", "-c", "import socket; socket.gethostbyname('" + name + "')"}, TimeoutMS: 20_000})
	e.Must(err)
	return res.ExitCode == 0
}

func egressWorkspace(e *Env, name string, allow ...string) *client.Workspace {
	e.T.Helper()
	ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: name, Egress: allow})
	e.Must(err)
	if len(ws.Egress) != len(allow) {
		e.T.Fatalf("workspace reports egress %v, want %v", ws.Egress, allow)
	}
	return ws
}

func init() {
	register("G5.4.1", func(e *Env) {
		world := newEgressNet(e)
		_ = world
		ws := egressWorkspace(e, "allowed", "allowed.test", "*.ok.test")
		if out, ok := fetch(e, ws.ID, "http://allowed.test:8080/"); !ok {
			e.T.Fatalf("fetch from an allowed name failed: %s", out)
		}
		if out, ok := fetch(e, ws.ID, "http://x.ok.test:8080/"); !ok {
			e.T.Fatalf("fetch from a name under an allowed suffix failed: %s", out)
		}
	})

	register("G5.4.2", func(e *Env) {
		newEgressNet(e)
		ws := egressWorkspace(e, "offlist", "allowed.test")
		if resolves(e, ws.ID, "other.test") {
			e.T.Fatal("a name off the list resolved")
		}
		if resolves(e, ws.ID, "ok.test") {
			e.T.Fatal("a name that only an unlisted suffix covers resolved")
		}
		// asking another DNS server directly: no answer gets through
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"python3", "-c",
			"import socket; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3); s.sendto(b'\\x12\\x34\\x01\\x00\\x00\\x01\\x00\\x00\\x00\\x00\\x00\\x00\\x05other\\x04test\\x00\\x00\\x01\\x00\\x01', ('1.1.1.1', 53)); s.recvfrom(512)"},
			TimeoutMS: 20_000})
		e.Must(err)
		if res.ExitCode == 0 {
			e.T.Fatal("a DNS query to an outside server got an answer")
		}
	})

	register("G5.4.3", func(e *Env) {
		world := newEgressNet(e)
		ws := egressWorkspace(e, "direct", "allowed.test")
		if _, ok := fetch(e, ws.ID, "http://"+world.otherIP+":8080/"); ok {
			e.T.Fatal("a direct connection to an address no allowed name led to succeeded")
		}
		ranged := egressWorkspace(e, "ranged", world.otherIP+"/32")
		if out, ok := fetch(e, ranged.ID, "http://"+world.otherIP+":8080/"); !ok {
			e.T.Fatalf("an allowed range was unreachable: %s", out)
		}
		if _, ok := fetch(e, ranged.ID, "http://"+world.allowedIP+":8080/"); ok {
			e.T.Fatal("an address outside the allowed range was reachable")
		}
	})

	register("G5.4.4", func(e *Env) {
		newEgressNet(e)
		ws := egressWorkspace(e, "kept", "allowed.test")
		check := func(what, id string) {
			e.T.Helper()
			if out, ok := fetch(e, id, "http://allowed.test:8080/"); !ok {
				e.T.Fatalf("%s: allowed name failed: %s", what, out)
			}
			if resolves(e, id, "other.test") {
				e.T.Fatalf("%s: a name off the list resolved", what)
			}
		}
		check("new workspace", ws.ID)
		sv := e.Save(ws.ID)
		e.Resume(sv.ID)
		check("after a resume", ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		check("a copy", copies[0].ID)
		e.Restart()
		check("after an engine restart", ws.ID)
	})
}
