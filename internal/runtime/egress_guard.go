package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// egressGuard enforces one workspace's allow-list: its DNS forwarder and the
// firewall chain it opens addresses in.
type egressGuard struct {
	eg       Egress
	chain    string // iptables chain for the workspace's outbound traffic
	listen   string // host side of the workspace's link, port 53
	upstream []string
	log      *slog.Logger

	conn *net.UDPConn
	done chan struct{}

	mu      sync.Mutex
	learned map[netip.Addr]time.Time // addresses opened by DNS answers, until
}

// learnMin and learnMax bound how long an address from a DNS answer stays
// open for new connections (established ones stay open regardless).
const (
	learnMin = 60 * time.Second
	learnMax = time.Hour
)

func egressChain(spec Spec) string { return "CAIRN-" + shortHash(spec.ID, 10) }

// hostResolvers are the resolvers the host itself uses: systemd-resolved's
// upstream list where it runs (/etc/resolv.conf then only names its local
// stub, which a forwarder on another link cannot use), else /etc/resolv.conf,
// else public resolvers.
func hostResolvers() []string {
	for _, p := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		if data, err := os.ReadFile(p); err == nil && hasNameserver(data) {
			return upstreamResolvers(data)
		}
	}
	return upstreamResolvers(nil)
}

// hasNameserver reports whether a resolv.conf names a usable resolver.
func hasNameserver(data []byte) bool {
	r := upstreamResolvers(data)
	return !(len(r) == 2 && r[0] == "1.1.1.1:53" && r[1] == "8.8.8.8:53" && !bytes.Contains(data, []byte("1.1.1.1")))
}

// ipt runs iptables, waiting for the xtables lock.
func ipt(args ...string) error {
	out, err := exec.Command("iptables", append([]string{"-w"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// buildChain (re)creates the workspace's chain: established flows, the
// configured ranges, the addresses learned so far, then drop.
func (g *egressGuard) buildChain() error {
	_ = ipt("-N", g.chain) // exists already: fine
	if err := ipt("-F", g.chain); err != nil {
		return err
	}
	rules := [][]string{{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"}}
	for _, p := range g.eg.Prefixes {
		rules = append(rules, []string{"-d", p.String(), "-j", "ACCEPT"})
	}
	g.mu.Lock()
	for a, until := range g.learned {
		if time.Now().Before(until) {
			rules = append(rules, []string{"-d", a.String() + "/32", "-j", "ACCEPT"})
		}
	}
	g.mu.Unlock()
	rules = append(rules, []string{"-j", "DROP"})
	for _, r := range rules {
		if err := ipt(append([]string{"-A", g.chain}, r...)...); err != nil {
			return err
		}
	}
	return nil
}

// start listens for the workspace's DNS queries. Idempotent.
func (g *egressGuard) start() error {
	if g.conn != nil {
		return nil
	}
	addr, err := net.ResolveUDPAddr("udp4", g.listen)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("egress DNS for the workspace: %w", err)
	}
	g.conn, g.done = conn, make(chan struct{})
	go g.serve()
	go g.expire()
	return nil
}

func (g *egressGuard) stop() {
	if g.conn == nil {
		return
	}
	close(g.done)
	g.conn.Close()
	g.conn = nil
}

func (g *egressGuard) serve() {
	conn := g.conn
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return // closed
		}
		q := append([]byte{}, buf[:n]...)
		go func() {
			if resp := g.answer(q); resp != nil {
				_, _ = conn.WriteToUDP(resp, from)
			}
		}()
	}
}

// answer resolves one query: names off the list are refused; answers for
// names on it open their IPv4 addresses before they are returned.
func (g *egressGuard) answer(q []byte) []byte {
	name, _, err := dnsQuestion(q)
	if err != nil {
		return nil
	}
	if !g.eg.Allows(name) {
		g.log.Info("egress: name not allowed", "name", name)
		return dnsRefuse(q)
	}
	resp, err := g.forward(q)
	if err != nil {
		g.log.Warn("egress: upstream DNS failed", "name", name, "err", err)
		return nil // the workspace's resolver retries
	}
	addrs, ttl, err := dnsAnswersA(resp)
	if err != nil {
		return resp
	}
	life := min(max(time.Duration(ttl)*time.Second, learnMin), learnMax)
	for _, a := range addrs {
		if err := g.open(a, life); err != nil {
			g.log.Warn("egress: could not open an address", "name", name, "addr", a, "err", err)
		}
	}
	return resp
}

func (g *egressGuard) forward(q []byte) ([]byte, error) {
	var last error
	for _, up := range g.upstream {
		c, err := net.DialTimeout("udp", up, 2*time.Second)
		if err != nil {
			last = err
			continue
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write(q); err != nil {
			c.Close()
			last = err
			continue
		}
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		c.Close()
		if err != nil {
			last = err
			continue
		}
		return buf[:n], nil
	}
	if last == nil {
		last = errors.New("no upstream resolvers")
	}
	return nil, last
}

// open lets new connections to a reach a for life.
func (g *egressGuard) open(a netip.Addr, life time.Duration) error {
	g.mu.Lock()
	_, known := g.learned[a]
	g.learned[a] = time.Now().Add(life)
	g.mu.Unlock()
	if known {
		return nil
	}
	return ipt("-I", g.chain, "2", "-d", a.String()+"/32", "-j", "ACCEPT")
}

// expire closes learned addresses whose answers are no longer valid.
func (g *egressGuard) expire() {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-g.done:
			return
		case <-tick.C:
		}
		var gone []netip.Addr
		g.mu.Lock()
		for a, until := range g.learned {
			if time.Now().After(until) {
				gone = append(gone, a)
				delete(g.learned, a)
			}
		}
		g.mu.Unlock()
		for _, a := range gone {
			_ = ipt("-D", g.chain, "-d", a.String()+"/32", "-j", "ACCEPT")
		}
	}
}
