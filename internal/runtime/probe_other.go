//go:build !linux

package runtime

// probeRule: no raw-table rule off Linux.
var probeRule []string

// ProbeConnections needs Linux (raw sockets with marks); runsc runs nowhere else.
func (r *Runsc) ProbeConnections(spec Spec, conns []TCPConn) error { return ErrUnsupported }
