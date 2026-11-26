package runtime

import (
	"fmt"
	"syscall"
)

// probeMark marks probes so they bypass connection tracking (and with it
// NAT): one whose peer is another workspace would otherwise match that
// workspace's MASQUERADE rule and arrive from the wrong address.
const probeMark = 0xca17

// probeRule is the raw-table rule that keeps marked probes untracked.
var probeRule = []string{"-t", "raw", "OUTPUT", "-m", "mark", "--mark", fmt.Sprint(probeMark), "-j", "CT", "--notrack"}

// ProbeConnections sends each connection's probe from the host, through a
// raw socket (the engine runs as root), into the workspace's own link.
func (r *Runsc) ProbeConnections(spec Spec, conns []TCPConn) error {
	if !spec.Network || len(conns) == 0 {
		return nil
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		return fmt.Errorf("raw socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_MARK, probeMark); err != nil {
		return fmt.Errorf("mark probes: %w", err)
	}
	for _, c := range conns {
		if !c.Local.Addr().Is4() || !c.Remote.Addr().Is4() {
			continue
		}
		if err := syscall.Sendto(fd, probeSegment(c), 0, &syscall.SockaddrInet4{Addr: c.Local.Addr().As4()}); err != nil {
			return fmt.Errorf("probe %s -> %s: %w", c.Remote, c.Local, err)
		}
	}
	return nil
}
