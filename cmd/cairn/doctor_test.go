package main

import "testing"

func TestLowDisk(t *testing.T) {
	const gb = 1 << 30
	for _, c := range []struct {
		free, total int64
		low         bool
	}{
		{100 * gb, 200 * gb, false},
		{4 * gb, 1000 * gb, true},  // under 5 GB, however big the volume
		{15 * gb, 200 * gb, true},  // under 10%
		{25 * gb, 200 * gb, false}, // 12.5%
		{6 * gb, 40 * gb, false},
	} {
		if got := lowDisk(c.free, c.total); got != c.low {
			t.Errorf("lowDisk(%d GB free of %d GB) = %v, want %v", c.free/gb, c.total/gb, got, c.low)
		}
	}
}
