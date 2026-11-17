//go:build !linux && !darwin

package save

import (
	"io/fs"
	"time"
)

// Without a ctime every candidate file is hashed.
func changeTime(fs.FileInfo) time.Time { return time.Now() }

func ownerMatches(fs.FileInfo, IndexEntry) bool { return true }
