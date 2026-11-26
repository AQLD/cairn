package runtime

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func TestParseProcNetTCP(t *testing.T) {
	listing := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1
   1: 02C8920A:B0F2 02C9920A:238C 01 00000000:00000000 00:00000000 00000000     0        0 2
   2: 0100007F:9C40 0100007F:1F90 01 00000000:00000000 00:00000000 00000000     0        0 3
   3: 02C8920A:B0F4 01010101:01BB 08 00000000:00000000 00:00000000 00000000     0        0 4
`
	got := ParseProcNetTCP(listing)
	want := []TCPConn{{Local: netip.MustParseAddrPort("10.146.200.2:45298"), Remote: netip.MustParseAddrPort("10.146.201.2:9100")}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v (listening, loopback and closing ones left out)", got, want)
	}
}

func TestProbeSegmentChecksums(t *testing.T) {
	c := TCPConn{Local: netip.MustParseAddrPort("10.200.1.2:45298"), Remote: netip.MustParseAddrPort("1.1.1.1:443")}
	pkt := probeSegment(c)
	if checksum(pkt[:20], 0) != 0 {
		t.Fatal("IP header checksum does not verify")
	}
	var sum uint32
	for i := 12; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(pkt[i:]))
	}
	if checksum(pkt[20:], sum+6+20) != 0 {
		t.Fatal("TCP checksum does not verify")
	}
	if binary.BigEndian.Uint16(pkt[20:]) != 443 || binary.BigEndian.Uint16(pkt[22:]) != 45298 || pkt[33] != 0x10 {
		t.Fatalf("segment is not an ACK from the peer to the workspace: % x", pkt[20:40])
	}
}
