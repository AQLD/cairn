package acceptance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// G5.6: the API over TLS. Each scenario makes its own CA and a certificate
// for 127.0.0.1, and runs a cairnd process with them.

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	file string // the CA certificate, PEM
}

func newTestCA(e *Env, dir, name string) *testCA {
	e.T.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	e.Must(err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	e.Must(err)
	cert, err := x509.ParseCertificate(der)
	e.Must(err)
	file := filepath.Join(dir, name+"-ca.pem")
	e.Must(os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	return &testCA{cert: cert, key: key, file: file}
}

// issue writes a certificate for 127.0.0.1 signed by the CA, and its key.
func (ca *testCA) issue(e *Env, certFile, keyFile string) {
	e.T.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	e.Must(err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "cairnd"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	e.Must(err)
	kder, err := x509.MarshalECPrivateKey(key)
	e.Must(err)
	// written next to the target and renamed, as renewal tools do
	e.Must(os.WriteFile(keyFile+".new", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600))
	e.Must(os.WriteFile(certFile+".new", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	e.Must(os.Rename(keyFile+".new", keyFile))
	e.Must(os.Rename(certFile+".new", certFile))
}

type tlsEngine struct {
	proc              *exec.Cmd
	addr              string
	url               string
	certFile, keyFile string
	ca                *testCA
	dir               string
}

// startTLSEngine runs cairnd over HTTPS with a certificate from a new CA.
func startTLSEngine(e *Env) *tlsEngine {
	e.T.Helper()
	dir := e.T.TempDir()
	te := &tlsEngine{dir: dir, certFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key")}
	te.ca = newTestCA(e, dir, "first")
	te.ca.issue(e, te.certFile, te.keyFile)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	e.Must(err)
	te.addr = ln.Addr().String()
	ln.Close()
	te.url = "https://" + te.addr
	te.proc = exec.Command(buildCairnd(e), append(cairndRuntimeArgs(testInit(e.T)), "--data", filepath.Join(dir, "data"), "--listen", te.addr,
		"--pause-on-shutdown=false", "--tls-cert", te.certFile, "--tls-key", te.keyFile)...)
	e.Must(te.proc.Start())
	e.T.Cleanup(func() {
		_ = te.proc.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = te.proc.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = te.proc.Process.Kill()
			<-done
		}
	})
	c := te.client(e, te.ca.file)
	deadline := time.Now().Add(60 * time.Second)
	for c.Health(e.Ctx) != nil {
		if time.Now().After(deadline) {
			e.T.Fatalf("cairnd did not come up over HTTPS on %s", te.addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return te
}

func (te *tlsEngine) client(e *Env, caFile string) *client.Client {
	e.T.Helper()
	c := client.New(te.url, "")
	if caFile == "" {
		return c
	}
	c, err := c.WithCA(caFile)
	e.Must(err)
	return c
}

// servedBy reports which CA signed the certificate the engine serves now.
func (te *tlsEngine) servedBy(e *Env) string {
	e.T.Helper()
	conn, err := tls.Dial("tcp", te.addr, &tls.Config{InsecureSkipVerify: true})
	e.Must(err)
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Issuer.CommonName
}

func init() {
	register("G5.6.1", func(e *Env) {
		te := startTLSEngine(e)
		c := te.client(e, te.ca.file)
		ws, err := c.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "over-tls"})
		e.Must(err)
		res, err := c.Run(e.Ctx, ws.ID, "echo", "encrypted")
		e.Must(err)
		Equal(e, "command output over HTTPS", strings.TrimSpace(res), "encrypted")
		// plain HTTP to the same port: no API
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + te.addr + "/v1/workspaces")
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				e.T.Fatal("the API answered plain HTTP on the TLS port")
			}
		}
	})

	register("G5.6.2", func(e *Env) {
		te := startTLSEngine(e)
		if err := te.client(e, "").Health(e.Ctx); err == nil || !strings.Contains(err.Error(), "certificate") {
			e.T.Fatalf("a client without the CA connected (or failed for another reason): %v", err)
		}
		other := newTestCA(e, te.dir, "other")
		if err := te.client(e, other.file).Health(e.Ctx); err == nil {
			e.T.Fatal("a client trusting another CA connected")
		}
		// a key that is not the certificate's: refused at start
		bad := filepath.Join(te.dir, "bad")
		e.Must(os.Mkdir(bad, 0o700))
		other.issue(e, filepath.Join(bad, "tls.crt"), filepath.Join(bad, "unused.key"))
		out, err := exec.Command(buildCairnd(e), append(cairndRuntimeArgs(testInit(e.T)), "--data", filepath.Join(bad, "data"), "--listen", "127.0.0.1:0",
			"--tls-cert", filepath.Join(bad, "tls.crt"), "--tls-key", te.keyFile)...).CombinedOutput()
		if err == nil {
			e.T.Fatal("cairnd started with a key that does not match its certificate")
		}
		if !strings.Contains(string(out), "TLS certificate") {
			e.T.Fatalf("cairnd's refusal does not name the certificate: %s", out)
		}
	})

	register("G5.6.3", func(e *Env) {
		te := startTLSEngine(e)
		pid := te.proc.Process.Pid
		Equal(e, "first certificate", te.servedBy(e), "first")
		renewed := newTestCA(e, te.dir, "renewed")
		renewed.issue(e, te.certFile, te.keyFile)
		deadline := time.Now().Add(10 * time.Second)
		for te.servedBy(e) != "renewed" {
			if time.Now().After(deadline) {
				e.T.Fatal("the renewed certificate was not served 10 s after it was written")
			}
			time.Sleep(200 * time.Millisecond)
		}
		e.Must(te.client(e, renewed.file).Health(e.Ctx))
		Equal(e, "same engine process", te.proc.Process.Pid, pid)
		// a broken renewal: the last good certificate keeps serving
		e.Must(os.WriteFile(te.certFile, []byte("-----BEGIN CERTIFICATE-----\nhalf"), 0o644))
		time.Sleep(2 * time.Second)
		e.Must(te.client(e, renewed.file).Health(e.Ctx))
		Equal(e, "after a broken renewal", te.servedBy(e), "renewed")
	})

	register("G5.6.4", func(e *Env) {
		te := startTLSEngine(e)
		cli := func(ca string, args ...string) (string, error) {
			cmd := exec.CommandContext(e.Ctx, cairnCLI(e), args...)
			cmd.Env = append(os.Environ(), "CAIRN_ENDPOINT="+te.url, "CAIRN_TOKEN=", "CAIRN_CA_CERT="+ca, "CAIRN_CONFIG="+filepath.Join(te.dir, "none.json"))
			out, err := cmd.CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
		id, err := cli(te.ca.file, "create", "--name", "cli-tls")
		if err != nil {
			e.T.Fatalf("cairn create over HTTPS: %v: %s", err, id)
		}
		if out, err := cli(te.ca.file, "exec", id, "--", "echo", "via-cli"); err != nil || !strings.Contains(out, "via-cli") {
			e.T.Fatalf("cairn exec over HTTPS: %v: %s", err, out)
		}
		if out, err := cli("", "ls"); err == nil {
			e.T.Fatalf("the CLI connected without the CA: %s", out)
		}
		py := func(ca string) (string, error) {
			cmd := exec.CommandContext(e.Ctx, "python3", "-I", "-c",
				"import sys; sys.path.insert(0, sys.argv[1]); import cairn; c = cairn.Client(); print(len(c.workspaces()))", sdkPath(e))
			cmd.Env = append(os.Environ(), "CAIRN_ENDPOINT="+te.url, "CAIRN_TOKEN=", "CAIRN_CA_CERT="+ca, "CAIRN_CONFIG="+filepath.Join(te.dir, "none.json"))
			out, err := cmd.CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
		if out, err := py(te.ca.file); err != nil || out != "1" {
			e.T.Fatalf("Python SDK over HTTPS: %v: %s", err, out)
		}
		if out, err := py(""); err == nil {
			e.T.Fatalf("the Python SDK connected without the CA: %s", out)
		}
	})
}

func sdkPath(e *Env) string {
	p, err := filepath.Abs("../sdk/python")
	e.Must(err)
	return p
}
