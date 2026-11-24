package runtime

// Outbound allow-lists. A workspace created with an egress list reaches only
// the names and address ranges on it: its DNS goes to a forwarder the engine
// runs for it, which answers only allowed names and lets the firewall accept
// the addresses in those answers for as long as they are valid; everything
// else, a direct connection to an address included, is dropped.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Egress is a parsed allow-list.
type Egress struct {
	Names    []string       // exact names ("pypi.org")
	Suffixes []string       // "*.github.com" as ".github.com": any name below it
	Prefixes []netip.Prefix // addresses and ranges ("10.0.0.0/8", "203.0.113.7")
}

// ParseEgress checks and parses allow-list entries: a name, "*." and a
// name, an IPv4 address or an IPv4 range.
func ParseEgress(entries []string) (Egress, error) {
	var eg Egress
	for _, raw := range entries {
		e := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		switch {
		case e == "":
			return Egress{}, errors.New("empty egress entry")
		case strings.Contains(e, "/"):
			p, err := netip.ParsePrefix(e)
			if err != nil || !p.Addr().Is4() {
				return Egress{}, fmt.Errorf("egress %q: not an IPv4 range", raw)
			}
			eg.Prefixes = append(eg.Prefixes, p.Masked())
		case isAddr(e):
			a, err := netip.ParseAddr(e)
			if err != nil || !a.Is4() {
				return Egress{}, fmt.Errorf("egress %q: not an IPv4 address", raw)
			}
			eg.Prefixes = append(eg.Prefixes, netip.PrefixFrom(a, 32))
		case strings.HasPrefix(e, "*."):
			if !validName(e[2:]) {
				return Egress{}, fmt.Errorf("egress %q: not a domain name", raw)
			}
			eg.Suffixes = append(eg.Suffixes, e[1:])
		default:
			if !validName(e) {
				return Egress{}, fmt.Errorf("egress %q: not a domain name, address or range", raw)
			}
			eg.Names = append(eg.Names, e)
		}
	}
	return eg, nil
}

func isAddr(s string) bool { return strings.Trim(s, "0123456789.") == "" || strings.Contains(s, ":") }

func validName(s string) bool {
	if len(s) == 0 || len(s) > 253 || !strings.Contains(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			return false
		}
	}
	return true
}

// Allows reports whether name may be resolved.
func (eg Egress) Allows(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, n := range eg.Names {
		if name == n {
			return true
		}
	}
	for _, s := range eg.Suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// --- DNS (only what the forwarder needs) -------------------------------------

const (
	dnsTypeA        = 1
	dnsRcodeRefused = 5
)

// dnsQuestion returns the name and type of a query's first question.
func dnsQuestion(msg []byte) (string, uint16, error) {
	if len(msg) < 12 || binary.BigEndian.Uint16(msg[4:6]) == 0 {
		return "", 0, errors.New("dns: no question")
	}
	name, off, err := dnsName(msg, 12)
	if err != nil || off+4 > len(msg) {
		return "", 0, errors.New("dns: bad question")
	}
	return name, binary.BigEndian.Uint16(msg[off : off+2]), nil
}

// dnsRefuse turns a query into a REFUSED answer to it.
func dnsRefuse(query []byte) []byte {
	out := append([]byte{}, query...)
	if len(out) < 12 {
		return out
	}
	out[2] |= 0x80                            // QR: a response
	out[3] = out[3]&0xf0 | dnsRcodeRefused    // RA unchanged, rcode REFUSED
	binary.BigEndian.PutUint16(out[6:8], 0)   // no answers
	binary.BigEndian.PutUint16(out[8:10], 0)  // no authority
	binary.BigEndian.PutUint16(out[10:12], 0) // no additional
	if _, off, err := dnsName(out, 12); err == nil && off+4 <= len(out) {
		out = out[:off+4] // the question only
	}
	return out
}

// dnsAnswersA lists the IPv4 addresses in a response's answers, with the
// smallest TTL among them (seconds).
func dnsAnswersA(msg []byte) ([]netip.Addr, uint32, error) {
	if len(msg) < 12 {
		return nil, 0, errors.New("dns: short response")
	}
	qd, an := int(binary.BigEndian.Uint16(msg[4:6])), int(binary.BigEndian.Uint16(msg[6:8]))
	off := 12
	for range qd {
		_, o, err := dnsName(msg, off)
		if err != nil || o+4 > len(msg) {
			return nil, 0, errors.New("dns: bad question")
		}
		off = o + 4
	}
	var addrs []netip.Addr
	minTTL := uint32(0)
	for range an {
		_, o, err := dnsName(msg, off)
		if err != nil || o+10 > len(msg) {
			return nil, 0, errors.New("dns: bad answer")
		}
		typ := binary.BigEndian.Uint16(msg[o : o+2])
		ttl := binary.BigEndian.Uint32(msg[o+4 : o+8])
		rdlen := int(binary.BigEndian.Uint16(msg[o+8 : o+10]))
		rd := o + 10
		if rd+rdlen > len(msg) {
			return nil, 0, errors.New("dns: bad answer data")
		}
		if typ == dnsTypeA && rdlen == 4 {
			addrs = append(addrs, netip.AddrFrom4([4]byte(msg[rd:rd+4])))
			if minTTL == 0 || ttl < minTTL {
				minTTL = ttl
			}
		}
		off = rd + rdlen
	}
	return addrs, minTTL, nil
}

// dnsName reads a (possibly compressed) name at off and returns it with the
// offset just after it.
func dnsName(msg []byte, off int) (string, int, error) {
	var labels []string
	end := -1
	for jumps := 0; ; {
		if off >= len(msg) {
			return "", 0, errors.New("dns: name past the end")
		}
		l := int(msg[off])
		switch {
		case l == 0:
			if end < 0 {
				end = off + 1
			}
			return strings.Join(labels, "."), end, nil
		case l&0xc0 == 0xc0:
			if off+1 >= len(msg) || jumps > 20 {
				return "", 0, errors.New("dns: bad pointer")
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:off+2]) & 0x3fff)
			jumps++
		default:
			if off+1+l > len(msg) {
				return "", 0, errors.New("dns: label past the end")
			}
			labels = append(labels, strings.ToLower(string(msg[off+1:off+1+l])))
			off += 1 + l
		}
	}
}

// upstreamResolvers are the host's own resolvers (from /etc/resolv.conf),
// or public ones when it has none usable.
func upstreamResolvers(resolvConf []byte) []string {
	var out []string
	for _, line := range strings.Split(string(resolvConf), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() && !a.IsLoopback() {
				out = append(out, net.JoinHostPort(f[1], "53"))
			}
		}
	}
	if len(out) == 0 {
		out = []string{"1.1.1.1:53", "8.8.8.8:53"}
	}
	return out
}
