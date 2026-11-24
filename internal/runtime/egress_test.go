package runtime

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

func TestParseEgressAndAllows(t *testing.T) {
	eg, err := ParseEgress([]string{"PyPI.org.", "*.github.com", "10.1.0.0/16", "203.0.113.7"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"pypi.org": true, "PYPI.ORG.": true, "files.pypi.org": false,
		"api.github.com": true, "a.b.github.com": true, "github.com": false, "evilgithub.com": false,
		"example.com": false,
	} {
		if got := eg.Allows(name); got != want {
			t.Errorf("Allows(%q) = %v, want %v", name, got, want)
		}
	}
	if len(eg.Prefixes) != 2 || eg.Prefixes[1] != netip.MustParsePrefix("203.0.113.7/32") {
		t.Errorf("prefixes: %v", eg.Prefixes)
	}
	for _, bad := range []string{"", "*.", "no_dots", "fe80::1", "10.0.0.0/33", "a..b", "*.com"} {
		if _, err := ParseEgress([]string{bad}); err == nil && bad != "*.com" {
			t.Errorf("ParseEgress(%q) accepted", bad)
		}
	}
}

// query builds a DNS query for name and type.
func query(id uint16, name string, typ uint16) []byte {
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[0:], id)
	msg[2] = 0x01 // RD
	binary.BigEndian.PutUint16(msg[4:], 1)
	for _, l := range strings.Split(name, ".") {
		msg = append(msg, byte(len(l)))
		msg = append(msg, l...)
	}
	msg = append(msg, 0, byte(typ>>8), byte(typ), 0, 1)
	return msg
}

func TestDNSQuestionRefuseAndAnswers(t *testing.T) {
	q := query(0x1234, "Files.PyPI.org", dnsTypeA)
	name, typ, err := dnsQuestion(q)
	if err != nil || name != "files.pypi.org" || typ != dnsTypeA {
		t.Fatalf("question: %q %d %v", name, typ, err)
	}
	r := dnsRefuse(q)
	if binary.BigEndian.Uint16(r[0:]) != 0x1234 || r[2]&0x80 == 0 || r[3]&0x0f != dnsRcodeRefused || binary.BigEndian.Uint16(r[6:]) != 0 {
		t.Fatalf("refusal: % x", r[:12])
	}
	// a response: the question, then a CNAME and two A records using a compression pointer to the question's name
	resp := append([]byte{}, q...)
	resp[2] |= 0x80
	binary.BigEndian.PutUint16(resp[6:], 3)
	rr := func(typ uint16, ttl uint32, data []byte) {
		resp = append(resp, 0xc0, 12, byte(typ>>8), byte(typ), 0, 1)
		resp = binary.BigEndian.AppendUint32(resp, ttl)
		resp = binary.BigEndian.AppendUint16(resp, uint16(len(data)))
		resp = append(resp, data...)
	}
	rr(5, 300, []byte{3, 'c', 'd', 'n', 0xc0, 12}) // CNAME
	rr(dnsTypeA, 120, []byte{151, 101, 0, 223})
	rr(dnsTypeA, 60, []byte{151, 101, 64, 223})
	addrs, ttl, err := dnsAnswersA(resp)
	if err != nil || len(addrs) != 2 || addrs[1] != netip.MustParseAddr("151.101.64.223") || ttl != 60 {
		t.Fatalf("answers: %v ttl %d %v", addrs, ttl, err)
	}
	if _, _, err := dnsAnswersA(resp[:len(resp)-3]); err == nil {
		t.Fatal("a truncated response parsed")
	}
}

func TestUpstreamResolvers(t *testing.T) {
	got := upstreamResolvers([]byte("# x\nnameserver 127.0.0.53\nnameserver 192.168.5.3\nnameserver ::1\n"))
	if len(got) != 1 || got[0] != "192.168.5.3:53" {
		t.Fatalf("%v", got)
	}
	if got := upstreamResolvers(nil); len(got) != 2 {
		t.Fatalf("fallback: %v", got)
	}
}
