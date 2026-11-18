//go:build linux || darwin

package save

import (
	"io/fs"
	"syscall"
	"time"
)

// changeTime is a file's inode change time (ctime): moved to "now" by any
// write or metadata change, and impossible to set back.
func changeTime(info fs.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Now() // unknown: treat as just changed
	}
	return ctime(st)
}

func ownerMatches(info fs.FileInfo, e IndexEntry) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == e.UID && int(st.Gid) == e.GID
}
