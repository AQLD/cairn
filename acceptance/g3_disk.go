package acceptance

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/AQLD/cairn/client"
)

// G3.2.3 and G3.2.6: a workspace's disk has the size it was given.

const diskTestMB = 512

func init() {
	register("G3.2.3", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "small-disk", Limits: client.Limits{DiskMB: diskTestMB}})
		e.Must(err)
		Equal(e, "size reported", ws.Limits.DiskMB, int64(diskTestMB))
		neighbour := e.Workspace("neighbour")

		// a command filling the disk fails like on a full disk...
		fillDisk(e, ws.ID)
		// ...and so does a write through the API, with a code saying why
		err = e.C.WriteFile(e.Ctx, ws.ID, "more", bytes.Repeat([]byte("x"), 8<<20))
		e.ErrCode(err, "insufficient_storage")
		if err != nil && !strings.Contains(err.Error(), "disk is full") {
			e.T.Fatalf("error does not say the disk is full: %v", err)
		}
		// the neighbour's disk is its own
		e.Sh(neighbour.ID, "dd if=/dev/zero of=big bs=1M count=64 status=none && rm big")

		// freeing space makes the workspace writable again
		e.Sh(ws.ID, "rm -f fill more")
		e.Write(ws.ID, "after", "writable again")
		Equal(e, "write after freeing space", e.Read(ws.ID, "after"), "writable again")
	})

	register("G3.2.6", func(e *Env) {
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "kept-limit", Limits: client.Limits{DiskMB: diskTestMB}})
		e.Must(err)
		e.Write(ws.ID, "work.txt", "precious")
		sv := e.Save(ws.ID)

		e.Resume(sv.ID)
		Equal(e, "files after resume", e.Read(ws.ID, "work.txt"), "precious")
		fillDisk(e, ws.ID)
		e.Sh(ws.ID, "rm -f fill") // the copy is made from the save, not from this

		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		cp := copies[0]
		Equal(e, "copy's size", cp.Limits.DiskMB, int64(diskTestMB))
		Equal(e, "files in the copy", e.Read(cp.ID, "work.txt"), "precious")
		fillDisk(e, cp.ID)

		if e.Engine == nil {
			return // remote engine: cannot restart it
		}
		e.Restart()
		Equal(e, "files after restart", e.Read(ws.ID, "work.txt"), "precious")
		fillDisk(e, ws.ID)
		e.Sh(ws.ID, "rm -f fill")
		e.Write(ws.ID, "after", "writable")

		// a host reboot: sandboxes and mounts are gone; the engine mounts every
		// workspace's disk again before anything uses it
		e.Must(e.Engine.StopAll(e.Ctx))
		unmountAll(e.DataDir)
		e.Restart()
		Equal(e, "files after reboot", e.Read(ws.ID, "after"), "writable")
		Equal(e, "copy's files after reboot", e.Read(cp.ID, "work.txt"), "precious")
		fillDisk(e, ws.ID)
	})
}

// fillDisk writes the file "fill" until the workspace's disk is full and checks
// that this failed like a full disk, within the workspace's size.
func fillDisk(e *Env, ws string) {
	e.T.Helper()
	res, err := e.C.Exec(e.Ctx, ws, client.Exec{
		Command:   []string{"sh", "-c", "dd if=/dev/zero of=fill bs=1M; ls -l fill"},
		TimeoutMS: 120_000,
	})
	e.Must(err)
	if !strings.Contains(res.Stderr, "No space left on device") {
		e.T.Fatalf("filling the disk: want \"No space left on device\", got exit %d, stderr %q", res.ExitCode, res.Stderr)
	}
	// ls -l: mode links owner group SIZE ...
	fields := strings.Fields(res.Stdout)
	if len(fields) < 5 {
		e.T.Fatalf("ls -l fill: %q", res.Stdout)
	}
	size, err := strconv.ParseInt(fields[4], 10, 64)
	e.Must(err)
	if size > diskTestMB<<20 {
		e.T.Fatalf("wrote %d MB into a %d MB disk", size>>20, diskTestMB)
	}
}
