package acceptance

import (
	"syscall"

	"github.com/AQLD/cairn/client"
)

// G8.5: a workspace filling its disk costs the host at most that disk, and
// nobody else notices.

func init() {
	register("G8.5", func(e *Env) {
		neighbour := e.Workspace("neighbour")
		freeBefore := freeMB(e)
		hog, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "hog", Limits: client.Limits{DiskMB: diskTestMB}})
		e.Must(err)

		fillDisk(e, hog.ID)
		used := freeBefore - freeMB(e)
		// the template's files plus the fill, never more than the disk's size
		// (with slack for the engine's own writes and filesystem metadata)
		if used > diskTestMB+64 {
			e.T.Fatalf("a %d MB workspace took %d MB of the host's disk", diskTestMB, used)
		}

		e.Sh(neighbour.ID, "dd if=/dev/zero of=big bs=1M count=128 status=none")
		e.Save(neighbour.ID)
		// the full workspace can still be saved, resumed and copied
		sv := e.Save(hog.ID)
		e.Resume(sv.ID)
		Equal(e, "hog answers after resume", e.Sh(hog.ID, "echo ok"), "ok\n")
		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		Equal(e, "copy of the full disk answers", e.Sh(copies[0].ID, "echo ok"), "ok\n")
	})
}

// freeMB is the free space on the filesystem holding the engine's data.
func freeMB(e *Env) int64 {
	e.T.Helper()
	var st syscall.Statfs_t
	e.Must(syscall.Statfs(e.DataDir, &st))
	return int64(st.Bavail) * int64(st.Bsize) >> 20 //nolint:unconvert // field types differ by OS
}
