package acceptance

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
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
