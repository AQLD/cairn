package save

import (
	"syscall"
	"time"
)

func ctime(st *syscall.Stat_t) time.Time { return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec) }
