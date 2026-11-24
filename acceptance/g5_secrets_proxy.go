package acceptance

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
)

// G5.9 through the proxy: HTTPS servers in the test's network namespace (on
// both its addresses) record what they receive; they answer with a hash of
// each credential header, so no value ever travels back into a workspace.

const tlsEchoServer = `
import hashlib, http.server, json, ssl, sys
cert, key, addr, log = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(log, "a") as f:
            f.write(repr(dict(self.headers)) + "\n")
        out = {k.lower(): hashlib.sha256(v.encode()).hexdigest() for k, v in self.headers.items()
               if k.lower() in ("authorization", "x-api-key")}
        body = json.dumps(out).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
srv = http.server.ThreadingHTTPServer((addr, 8443), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
srv.serve_forever()
`

type secretWorld struct {
	*egressNet
	logs     map[string]string // address → file of the headers it received
	hostOnly string            // an HTTPS server on the host's loopback, trusted by the proxy
}

func newSecretWorld(e *Env) *secretWorld {
	e.T.Helper()
	world := newEgressNet(e)
	dir := e.T.TempDir()
	ca := newTestCA(e, dir, "upstream")
	certFile, keyFile := filepath.Join(dir, "srv.crt"), filepath.Join(dir, "srv.key")
	ca.issueFor(e, certFile, keyFile, world.allowedIP, world.otherIP)
	script := filepath.Join(dir, "echo.py")
	e.Must(os.WriteFile(script, []byte(tlsEchoServer), 0o644))
	sw := &secretWorld{egressNet: world, logs: map[string]string{}}
	// a service on the host itself, which no workspace may reach through the proxy
	lc, lk := filepath.Join(dir, "lo.crt"), filepath.Join(dir, "lo.key")
	ca.issueFor(e, lc, lk, "127.0.0.1")
	pair, err := tls.LoadX509KeyPair(lc, lk)
	e.Must(err)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	e.Must(err)
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("host service")) })}
	go func() { _ = hs.Serve(ln) }()
	e.T.Cleanup(func() { hs.Close() })
	sw.hostOnly = "https://" + ln.Addr().String() + "/"
	for _, ip := range []string{world.allowedIP, world.otherIP} {
		log := filepath.Join(dir, ip+".log")
		e.Must(os.WriteFile(log, nil, 0o644))
		sw.logs[ip] = log
		srv := exec.Command("ip", "netns", "exec", world.ns, "python3", script, certFile, keyFile, ip, log)
		e.Must(srv.Start())
		e.T.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	}
	// a plain server on 443 too, to prove a direct connection gets nowhere
	plain := exec.Command("ip", "netns", "exec", world.ns, "python3", "-m", "http.server", "443", "--bind", world.otherIP)
	e.Must(plain.Start())
	e.T.Cleanup(func() { _ = plain.Process.Kill(); _ = plain.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for _, addr := range []string{world.allowedIP + ":8443", world.otherIP + ":8443", world.otherIP + ":443"} {
		for {
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				c.Close()
				break
			}
			if time.Now().After(deadline) {
				e.T.Fatalf("test server %s did not start", addr)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	key := secretsKey(e)
	e.RestartWith(func(c *engine.Config) { c.SecretsKey = key; c.ProxyRoots = pool })
	return sw
}

// call makes an HTTPS request from inside the workspace with the given
// headers (values may name environment variables: $API_TOKEN) and returns
// the response body, or the error.
func call(e *Env, ws, url string, headers map[string]string) (string, bool) {
	e.T.Helper()
	var hs []string
	for k, v := range headers {
		hs = append(hs, "'"+k+"': os.path.expandvars('"+v+"')")
	}
	code := "import os, sys, base64, urllib.request\n" +
		"h = {" + strings.Join(hs, ", ") + "}\n" +
		"try:\n print(urllib.request.urlopen(urllib.request.Request('" + url + "', headers=h), timeout=10).read().decode())\n" +
		"except Exception as x:\n print(type(x).__name__, x); sys.exit(1)"
	// every request through the proxy, the host's loopback included
	res, err := e.C.Exec(e.Ctx, ws, client.Exec{Command: []string{"env", "-u", "NO_PROXY", "-u", "no_proxy", "python3", "-c", code}, TimeoutMS: 30_000})
	e.Must(err)
	return strings.TrimSpace(res.Stdout + res.Stderr), res.ExitCode == 0
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func init() {
	register("G5.9.1", func(e *Env) {
		w := newSecretWorld(e)
		value := secretValue()
		_, err := e.C.SetSecret(e.Ctx, "API_TOKEN", value, []string{w.allowedIP})
		e.Must(err)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "uses-secret", Secrets: []string{"API_TOKEN"}})
		e.Must(err)
		Equal(e, "workspace lists its secrets", strings.Join(ws.Secrets, ","), "API_TOKEN")
		// what the workspace holds: a placeholder
		ph := strings.TrimSpace(e.Sh(ws.ID, "printf %s \"$API_TOKEN\""))
		if !strings.HasPrefix(ph, "cairn-secret-") || strings.Contains(ph, value) {
			e.T.Fatalf("the workspace's API_TOKEN is %q, want a placeholder", ph)
		}
		if strings.Contains(e.Sh(ws.ID, "cat /proc/self/environ | tr '\\0' '\\n'"), value) {
			e.T.Fatal("the value is in a process's environment")
		}
		// what the server receives: the value, in each kind of header
		out, ok := call(e, ws.ID, "https://"+w.allowedIP+":8443/", map[string]string{
			"Authorization": "Bearer $API_TOKEN", "X-Api-Key": "$API_TOKEN"})
		if !ok {
			e.T.Fatalf("request through the proxy failed: %s", out)
		}
		if !strings.Contains(out, sha("Bearer "+value)) || !strings.Contains(out, sha(value)) {
			e.T.Fatalf("the server did not receive the value (got hashes %s)", out)
		}
		basic, ok := call(e, ws.ID, "https://"+w.allowedIP+":8443/", map[string]string{
			"Authorization": "Basic ' + base64.b64encode(('x:' + os.environ['API_TOKEN']).encode()).decode() + '"})
		if !ok {
			e.T.Fatalf("basic auth request failed: %s", basic)
		}
		if !strings.Contains(basic, sha("Basic "+b64("x:"+value))) {
			e.T.Fatalf("basic auth did not carry the value (got %s)", basic)
		}
		if strings.Contains(out+basic, value) {
			e.T.Fatal("the value came back into the workspace")
		}
	})

	register("G5.9.3", func(e *Env) {
		w := newSecretWorld(e)
		value := secretValue()
		_, err := e.C.SetSecret(e.Ctx, "API_TOKEN", value, []string{w.allowedIP})
		e.Must(err)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "exfil", Secrets: []string{"API_TOKEN"}})
		e.Must(err)
		out, ok := call(e, ws.ID, "https://"+w.otherIP+":8443/", map[string]string{"Authorization": "Bearer $API_TOKEN"})
		if ok || !strings.Contains(out, "403") {
			e.T.Fatalf("a placeholder sent to a host off the secret's list was not refused: %s", out)
		}
		// without the placeholder the same host is fine: the proxy refuses the secret, not the host
		if out, ok := call(e, ws.ID, "https://"+w.otherIP+":8443/", map[string]string{"X-Other": "nothing"}); !ok {
			e.T.Fatalf("a request without a secret to another host failed: %s", out)
		}
		for ip, log := range w.logs {
			data, _ := os.ReadFile(log)
			if strings.Contains(string(data), value) && ip != w.allowedIP {
				e.T.Fatalf("server %s received the value", ip)
			}
		}
		evs, err := e.C.Events(e.Ctx, ws.ID, client.EventQuery{})
		e.Must(err)
		logged := false
		for _, ev := range evs {
			if ev.Kind == "secret_refused" && strings.Contains(ev.Summary, "API_TOKEN") && strings.Contains(ev.Summary, w.otherIP) {
				logged = true
			}
		}
		if !logged {
			e.T.Fatalf("the refusal is not in the activity log by secret name: %+v", evs)
		}
		// nor through the proxy to the host's own services or metadata
		for _, target := range []string{w.hostOnly, "https://169.254.169.254/"} {
			if out, ok := call(e, ws.ID, target, nil); ok {
				e.T.Fatalf("the proxy reached %s: %s", target, out)
			}
		}
	})

	register("G5.9.9", func(e *Env) {
		w := newSecretWorld(e)
		_, err := e.C.SetSecret(e.Ctx, "API_TOKEN", secretValue(), []string{w.allowedIP})
		e.Must(err)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "bypass", Secrets: []string{"API_TOKEN"}})
		e.Must(err)
		connect := func(port string) bool {
			res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"python3", "-c",
				"import socket; socket.create_connection(('" + w.otherIP + "', " + port + "), 4)"}, TimeoutMS: 20_000})
			e.Must(err)
			return res.ExitCode == 0
		}
		if !connect("8080") {
			e.T.Fatal("the workspace cannot reach another port directly (the test's network is broken)")
		}
		if connect("443") {
			e.T.Fatal("a direct connection on port 443 got through, around the proxy")
		}
	})
}

// scanFor reports the first file under dir that holds any needle, reading
// in chunks (disk images are large and sparse).
func scanFor(e *Env, dir string, needles ...string) string {
	e.T.Helper()
	longest := 0
	for _, n := range needles {
		longest = max(longest, len(n))
	}
	found := ""
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" || !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		buf := make([]byte, 4<<20)
		carry := 0
		for {
			n, err := f.Read(buf[carry:])
			chunk := buf[:carry+n]
			for _, nd := range needles {
				if bytes.Contains(chunk, []byte(nd)) {
					found = p
					return nil
				}
			}
			if err != nil {
				return nil
			}
			carry = min(longest, len(chunk))
			copy(buf, chunk[len(chunk)-carry:])
		}
	})
	return found
}

func init() {
	register("G5.9.2", func(e *Env) {
		w := newSecretWorld(e)
		value := secretValue()
		_, err := e.C.SetSecret(e.Ctx, "API_TOKEN", value, []string{w.allowedIP})
		e.Must(err)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "saved", Secrets: []string{"API_TOKEN"}, Limits: client.Limits{DiskMB: 512}})
		e.Must(err)
		// a process holding a known string in memory: a scan that cannot find it
		// in a full save proves nothing about the value
		canary := "canary-" + secretValue()
		e.Sh(ws.ID, "CANARY="+canary+" nohup sh -c 'while :; do sleep 1; done' >/dev/null 2>&1 &")
		// the secret in use: requests carry it, an agent's tools keep their token
		e.Sh(ws.ID, "printf 'machine x password %s\\n' \"$API_TOKEN\" > ~/.netrc")
		for i := 0; i < 3; i++ {
			if out, ok := call(e, ws.ID, "https://"+w.allowedIP+":8443/", map[string]string{"Authorization": "Bearer $API_TOKEN"}); !ok {
				e.T.Fatalf("request with the secret failed: %s", out)
			}
		}
		e.Save(ws.ID)
		full, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		copies, err := e.C.Copy(e.Ctx, full.ID, 1)
		e.Must(err)
		e.Resume(full.ID)
		if p := scanFor(e, filepath.Join(e.DataDir, "saves"), canary); p == "" {
			e.T.Fatal("the scan cannot see process memory in a full save (compressed image?): it would prove nothing")
		}
		needles := []string{value, b64(value), b64("Bearer " + value)}
		for what, dir := range map[string]string{"saves": filepath.Join(e.DataDir, "saves"), "workspaces (the copy's disk included)": filepath.Join(e.DataDir, "workspaces")} {
			if p := scanFor(e, dir, needles...); p != "" {
				e.T.Fatalf("%s: %s holds the secret's value", what, p)
			}
		}
		// and the copy still uses the secret
		if out, ok := call(e, copies[0].ID, "https://"+w.allowedIP+":8443/", map[string]string{"Authorization": "Bearer $API_TOKEN"}); !ok || !strings.Contains(out, sha("Bearer "+value)) {
			e.T.Fatalf("the copy cannot use the secret: %s", out)
		}
	})

	register("G5.9.8", func(e *Env) {
		w := newSecretWorld(e)
		value := secretValue()
		_, err := e.C.SetSecret(e.Ctx, "API_TOKEN", value, []string{w.allowedIP})
		e.Must(err)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "source", Secrets: []string{"API_TOKEN"}})
		e.Must(err)
		srcPH := strings.TrimSpace(e.Sh(ws.ID, "printf %s \"$API_TOKEN\""))
		sv := e.Save(ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 2)
		e.Must(err)
		seen := map[string]bool{srcPH: true}
		for _, c := range copies {
			Equal(e, "copy lists its secrets", strings.Join(c.Secrets, ","), "API_TOKEN")
			ph := strings.TrimSpace(e.Sh(c.ID, "printf %s \"$API_TOKEN\""))
			if !strings.HasPrefix(ph, "cairn-secret-") || seen[ph] {
				e.T.Fatalf("copy %s holds placeholder %q: not its own", c.ID, ph)
			}
			seen[ph] = true
			if out, ok := call(e, c.ID, "https://"+w.allowedIP+":8443/", map[string]string{"Authorization": "Bearer $API_TOKEN"}); !ok || !strings.Contains(out, sha("Bearer "+value)) {
				e.T.Fatalf("copy %s: its placeholder does not work: %s", c.ID, out)
			}
			// the source's placeholder, carried into the copy, swaps for nothing
			out, ok := call(e, c.ID, "https://"+w.allowedIP+":8443/", map[string]string{"Authorization": "Bearer " + srcPH})
			if !ok || strings.Contains(out, sha("Bearer "+value)) {
				e.T.Fatalf("copy %s: the source's placeholder was swapped for the value: %s", c.ID, out)
			}
		}
	})
}
