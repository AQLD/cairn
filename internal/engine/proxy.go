package engine

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AQLD/cairn/internal/runtime"
)

// The egress proxy of workspaces that use secrets (docs/design/secrets.md).
// The workspace holds a placeholder per secret; the proxy swaps it for the
// value in the headers of requests to the secret's hosts, refuses it on
// requests anywhere else, and terminates TLS with the workspace's own CA to
// see those headers. Upstream it is an ordinary verifying client.

// Workspace-side paths of the proxy's CA (Spec.MountDir is /run/cairn).
const (
	caInWorkspace     = "/run/cairn/ca.pem"
	bundleInWorkspace = "/run/cairn/ca-bundle.pem"
)

// newPlaceholder is a value that means nothing outside this engine.
func newPlaceholder() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return "cairn-secret-" + hex.EncodeToString(b)
}

// proxyDir holds the workspace's CA (host side) and what the workspace sees
// of it (mount/, shown at /run/cairn).
func (e *Engine) proxyDir(id string) string { return filepath.Join(e.workspaceDir(id), "proxy") }

// workspaceCA loads the workspace's CA, making it on first use.
func (e *Engine) workspaceCA(id string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	dir := e.proxyDir(id)
	certFile, keyFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	if pair, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
		if err == nil && ok {
			return cert, key, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "Cairn workspace proxy " + id},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	mount := filepath.Join(dir, "mount")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		return nil, nil, err
	}
	// the bundle: this CA and the host's own roots, for clients that replace
	// the system bundle with SSL_CERT_FILE
	bundle := append([]byte{}, certPEM...)
	for _, p := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"} {
		if b, err := os.ReadFile(p); err == nil {
			bundle = append(append(bundle, '\n'), b...)
			break
		}
	}
	for name, data := range map[string][]byte{
		filepath.Join(mount, "ca.pem"):        certPEM,
		filepath.Join(mount, "ca-bundle.pem"): bundle,
		keyFile:                               pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}),
		certFile:                              certPEM, // last: its presence means the set is complete
	} {
		mode := os.FileMode(0o644)
		if name == keyFile {
			mode = 0o600
		}
		if err := writeFileSync(name, data, mode); err != nil {
			return nil, nil, err
		}
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func writeFileSync(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// secretEnv is what a workspace with secrets sees: placeholders, and where
// its proxy's CA is for the tools that do not read the system bundle.
func secretEnv(spec Spec) map[string]string {
	env := map[string]string{
		"SSL_CERT_FILE":       bundleInWorkspace,
		"REQUESTS_CA_BUNDLE":  bundleInWorkspace,
		"CURL_CA_BUNDLE":      bundleInWorkspace,
		"GIT_SSL_CAINFO":      bundleInWorkspace,
		"NODE_EXTRA_CA_CERTS": caInWorkspace,
	}
	for name, ph := range spec.Placeholders {
		env[name] = ph
	}
	return env
}

// proxy is one workspace's proxy handler.
type proxy struct {
	e      *Engine
	ws     string
	spec   Spec
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	egress *runtime.Egress // the workspace's allow-list, if it has one

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// proxyHandler returns the proxy for a workspace with secrets.
func (e *Engine) proxyHandler(id string, spec Spec) (http.Handler, error) {
	cert, key, err := e.workspaceCA(id)
	if err != nil {
		return nil, fmt.Errorf("workspace proxy CA: %w", err)
	}
	p := &proxy{e: e, ws: id, spec: spec, caCert: cert, caKey: key, leaves: map[string]*tls.Certificate{}}
	if len(spec.Egress) > 0 {
		eg, err := runtime.ParseEgress(spec.Egress)
		if err != nil {
			return nil, err
		}
		p.egress = &eg
	}
	return p, nil
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	// a plain-HTTP request in proxy form: http://host/path
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "cairn proxy: only proxy requests", http.StatusBadRequest)
		return
	}
	p.forward(w, r, "http", r.URL.Host)
}

// connect terminates the client's TLS with a leaf for the host it asked for
// and serves the requests inside as https requests to that host.
func (p *proxy) connect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		http.Error(w, "cairn proxy: bad CONNECT target", http.StatusBadRequest)
		return
	}
	if msg := p.refuseHost(host); msg != "" {
		http.Error(w, msg, http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cairn proxy: cannot take over the connection", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	var client net.Conn = conn
	if buf.Reader.Buffered() > 0 {
		client = &prefixConn{Conn: conn, r: io.MultiReader(io.LimitReader(buf.Reader, int64(buf.Reader.Buffered())), conn)}
	}
	tlsConn := tls.Server(client, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.leaf(host)
		},
	})
	if err := tlsConn.Handshake(); err != nil {
		p.e.log.Debug("proxy: client TLS failed", "workspace", p.ws, "host", host, "err", err)
		return
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.forward(w, r, "https", target) }),
		ReadHeaderTimeout: 30 * time.Second,
	}
	_ = srv.Serve(&oneConnListener{conn: tlsConn})
}

// forward sends one request on with secrets swapped in, or refuses it.
func (p *proxy) forward(w http.ResponseWriter, r *http.Request, scheme, target string) {
	host := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	if msg := p.refuseHost(host); msg != "" {
		http.Error(w, msg, http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme, out.URL.Host, out.Host = scheme, target, target
	removeHopHeaders(out.Header)
	values, err := p.swap(r.Context(), out.Header, host)
	if err != nil {
		var refused errSecretRefused
		if errors.As(err, &refused) {
			p.e.note(context.WithoutCancel(r.Context()), p.ws, "secret_refused",
				fmt.Sprintf("secret %s refused for %s: not one of its hosts", refused.name, host), nil, map[string]any{"secret": refused.name, "host": host})
			http.Error(w, fmt.Sprintf("cairn proxy: secret %s may not be sent to %s", refused.name, host), http.StatusForbidden)
			return
		}
		http.Error(w, "cairn proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := p.e.proxyTransport().RoundTrip(out)
	if err != nil {
		http.Error(w, "cairn proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	removeHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, mask(v, values))
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

type errSecretRefused struct{ name string }

func (e errSecretRefused) Error() string { return "secret " + e.name + " refused" }

// swap replaces placeholders in header values with their secrets' values,
// for the secrets that may go to host. It returns the values it used (to mask
// them in the response) or errSecretRefused for a placeholder off its list.
func (p *proxy) swap(ctx context.Context, h http.Header, host string) (map[string]string, error) {
	used := map[string]string{} // value → placeholder
	for name, ph := range p.spec.Placeholders {
		present := false
		for k, vs := range h {
			for _, v := range vs {
				if strings.Contains(v, ph) || (strings.EqualFold(k, "Authorization") && basicContains(v, ph)) {
					present = true
				}
			}
		}
		if !present {
			continue
		}
		value, hosts, err := p.e.secretValue(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", name, err)
		}
		eg, err := runtime.ParseEgress(hosts)
		if err != nil || !hostAllowed(eg, host) {
			return nil, errSecretRefused{name}
		}
		v := string(value)
		used[v] = ph
		for k, vs := range h {
			for i, hv := range vs {
				if strings.EqualFold(k, "Authorization") && basicContains(hv, ph) {
					hv = basicReplace(hv, ph, v)
				}
				vs[i] = strings.ReplaceAll(hv, ph, v)
			}
		}
	}
	return used, nil
}

// mask puts placeholders back where an upstream echoes a value.
func mask(s string, values map[string]string) string {
	for v, ph := range values {
		s = strings.ReplaceAll(s, v, ph)
	}
	return s
}

func basicContains(v, ph string) bool {
	user, ok := basicDecode(v)
	return ok && strings.Contains(user, ph)
}

func basicReplace(v, ph, value string) string {
	user, ok := basicDecode(v)
	if !ok {
		return v
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(user, ph, value)))
}

func basicDecode(v string) (string, bool) {
	scheme, rest, ok := strings.Cut(v, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
	return string(b), err == nil
}

// hostAllowed: host matches a name or suffix, or is an address in a range.
func hostAllowed(eg runtime.Egress, host string) bool {
	if a, err := netip.ParseAddr(host); err == nil {
		for _, p := range eg.Prefixes {
			if p.Contains(a.Unmap()) {
				return true
			}
		}
		return false
	}
	return eg.Allows(host)
}

// refuseHost says why the proxy will not go to host, or "".
func (p *proxy) refuseHost(host string) string {
	if p.egress != nil && !hostAllowed(*p.egress, host) {
		return "cairn proxy: " + host + " is not on this workspace's allow-list"
	}
	return ""
}

// leaf is a certificate for host signed by the workspace's CA.
func (p *proxy) leaf(host string) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.leaves[host]; c != nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, p.caCert.Raw}, PrivateKey: key}
	p.leaves[host] = c
	return c, nil
}

// proxyTransport is how every workspace proxy reaches the outside: verifying
// TLS (Config.ProxyRoots replaces the host's roots, for tests) and never to
// an address a workspace may not reach directly.
func (e *Engine) proxyTransport() *http.Transport {
	e.proxyOnce.Do(func() {
		dialer := &net.Dialer{Timeout: 15 * time.Second, Control: refuseInternal}
		e.proxyTr = &http.Transport{
			DialContext:           dialer.DialContext,
			TLSClientConfig:       &tls.Config{RootCAs: e.cfg.ProxyRoots, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			MaxIdleConnsPerHost:   4,
			ForceAttemptHTTP2:     false,
		}
	})
	return e.proxyTr
}

// internalPrefixes are addresses the proxy never connects to: what the host's
// firewall keeps workspaces from (metadata, the host itself, other
// workspaces), checked on the address actually dialed, after DNS.
var internalPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("100.100.100.200/32"),
	netip.MustParsePrefix("10.200.0.0/16"), // workspace links
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fe80::/10"),
}

func refuseInternal(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	a := ap.Addr().Unmap()
	for _, p := range internalPrefixes {
		if p.Contains(a) {
			return fmt.Errorf("%s is not reachable from a workspace", a)
		}
	}
	if isHostAddr(a) {
		return fmt.Errorf("%s is this host's own address, not reachable from a workspace", a)
	}
	return nil
}

func isHostAddr(a netip.Addr) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // fail closed
	}
	for _, x := range addrs {
		if n, ok := x.(*net.IPNet); ok {
			if h, ok := netip.AddrFromSlice(n.IP); ok && h.Unmap() == a {
				return true
			}
		}
	}
	return false
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func removeHopHeaders(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, k := range strings.Split(f, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// oneConnListener serves exactly one connection.
type oneConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() {
		c = l.conn
		l.done = make(chan struct{})
	})
	if c != nil {
		return &closeNotifyConn{Conn: c, done: l.done}, nil
	}
	<-l.done // until the one connection closes
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

type closeNotifyConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *closeNotifyConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// prefixConn reads what the HTTP server had already buffered first.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }
