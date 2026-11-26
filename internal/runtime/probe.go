package runtime

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Probing a restored workspace's connections. A full save keeps TCP
// connections in gVisor's network stack, and a restore brings them back as
// they were. If the peer dropped one meanwhile (an idle timeout, a restart),
// what it sent then reached nobody, and the restored side would wait on it
// forever. So after a restore the host sends each outside connection one
// segment the workspace cannot accept (out of window). TCP answers that with
// an ACK (RFC 793, RFC 5961), which goes to the real peer: a peer that still
// has the connection ignores it, one that does not answers with a reset,
// and the program waiting on the connection gets an error at once.

// ParseProcNetTCP returns the established IPv4 connections in a
// /proc/net/tcp listing whose peer is not loopback (connections inside the
// workspace survive a save on both ends).
func ParseProcNetTCP(listing string) []TCPConn {
	var out []TCPConn
	sc := bufio.NewScanner(strings.NewReader(listing))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[3] != "01" { // 01: ESTABLISHED
			continue
		}
		local, err1 := procAddr(f[1])
		remote, err2 := procAddr(f[2])
		if err1 != nil || err2 != nil || remote.Addr().IsLoopback() || local.Addr().IsLoopback() {
			continue
		}
		out = append(out, TCPConn{Local: local, Remote: remote})
	}
	return out
}

// procAddr parses /proc/net/tcp's "0100007F:1F90" (address in host byte
// order, little-endian on every platform the engine runs on).
func procAddr(s string) (netip.AddrPort, error) {
	a, p, ok := strings.Cut(s, ":")
	if !ok || len(a) != 8 {
		return netip.AddrPort{}, fmt.Errorf("address %q", s)
	}
	b, err := hex.DecodeString(a)
	if err != nil {
		return netip.AddrPort{}, err
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{b[3], b[2], b[1], b[0]}), uint16(port)), nil
}

// probeSegment is an IPv4 TCP segment from the peer to the workspace that
// no receiver accepts: an ACK for data never sent, at a random sequence.
func probeSegment(c TCPConn) []byte {
	var r [8]byte
	_, _ = rand.Read(r[:])
	pkt := make([]byte, 40)
	ip, tcp := pkt[:20], pkt[20:]
	ip[0] = 0x45 // IPv4, 20-byte header
	binary.BigEndian.PutUint16(ip[2:], 40)
	binary.BigEndian.PutUint16(ip[6:], 0x4000) // don't fragment
	ip[8] = 64                                 // TTL
	ip[9] = 6                                  // TCP
	src, dst := c.Remote.Addr().As4(), c.Local.Addr().As4()
	copy(ip[12:16], src[:])
	copy(ip[16:20], dst[:])
	binary.BigEndian.PutUint16(ip[10:], checksum(ip, 0))
	binary.BigEndian.PutUint16(tcp[0:], c.Remote.Port())
	binary.BigEndian.PutUint16(tcp[2:], c.Local.Port())
	copy(tcp[4:12], r[:]) // sequence and acknowledgement: random, so out of window
	tcp[12] = 5 << 4      // 20-byte header
	tcp[13] = 0x10        // ACK
	binary.BigEndian.PutUint16(tcp[14:], 65535)
	// pseudo-header: addresses, protocol, TCP length
	var sum uint32
	for i := 12; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[i:]))
	}
	sum += 6 + 20
	binary.BigEndian.PutUint16(tcp[16:], checksum(tcp, sum))
	return pkt
}

func checksum(b []byte, sum uint32) uint16 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	for sum > 0xffff {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}
