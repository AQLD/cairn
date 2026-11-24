package runtime

import (
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// proxyServer serves a workspace's proxy (Spec.Proxy) on the host side of its
// link. The handler is swapped on every firewall pass, so it is always the
// engine's current one.
type proxyServer struct {
	srv     *http.Server
	handler atomic.Pointer[http.Handler]
}

// proxyRules: the workspace reaches its proxy on the host, and no web port
// directly (above the host DROP and the FORWARD accepts, which -I 1 puts after).
func proxyRules(spec Spec) [][]string {
	hostIf := "ch" + shortHash(spec.ID, 10)
	hostIP, _ := subnet(spec)
	return [][]string{
		{"INPUT", "-i", hostIf, "-d", hostIP, "-p", "tcp", "--dport", fmt.Sprint(ProxyPort), "-j", "ACCEPT"},
		{"FORWARD", "-i", hostIf, "-p", "tcp", "--dport", "443", "-j", "DROP"},
		{"FORWARD", "-i", hostIf, "-p", "tcp", "--dport", "80", "-j", "DROP"},
	}
}

func (r *Runsc) serveProxyLocked(spec Spec) error {
	if p := r.proxies[spec.ID]; p != nil {
		p.handler.Store(&spec.Proxy)
		return nil
	}
	hostIP, _ := subnet(spec)
	ln, err := net.Listen("tcp4", net.JoinHostPort(hostIP, fmt.Sprint(ProxyPort)))
	if err != nil {
		return fmt.Errorf("workspace proxy: %w", err)
	}
	p := &proxyServer{}
	p.handler.Store(&spec.Proxy)
	p.srv = &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { (*p.handler.Load()).ServeHTTP(w, req) }),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() { _ = p.srv.Serve(ln) }()
	if r.proxies == nil {
		r.proxies = map[string]*proxyServer{}
	}
	r.proxies[spec.ID] = p
	return nil
}
