package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/failpoint"
)

// G6.4.3: the engine's own volume really fills up (not a failpoint): the
// engine refuses new work cleanly, a workspace writing at that moment loses
// that write but not its filesystem, and everything works again once there is
// room. Workspace disks are sparse images, so their sizes can add up to more
// than the volume: this is the case that matters.

const fullVolumeMB = 1536

func init() {
	register("G6.4.3", func(e *Env) {
		if os.Geteuid() != 0 {
			e.T.Skip("needs root: the test mounts a small volume for the engine's data")
		}
		if _, err := exec.LookPath("mkfs.ext4"); err != nil {
			e.T.Skip("needs mkfs.ext4")
		}
		// the engine's data on a small volume of its own
		vol := e.T.TempDir()
		img, mnt := filepath.Join(vol, "volume.img"), filepath.Join(vol, "data")
		e.Must(os.Mkdir(mnt, 0o700))
		run(e, "truncate", "-s", fmt.Sprintf("%dM", fullVolumeMB), img)
		run(e, "mkfs.ext4", "-q", "-F", "-m", "0", img)
		run(e, "mount", "-o", "loop", img, mnt)
		e.T.Cleanup(func() {
			unmountAll(mnt)
			_ = exec.Command("umount", "-l", mnt).Run()
		})
		e.stop()
		orig := e.DataDir // runsc keeps its state here still
		e.T.Cleanup(func() { unmountAll(orig) })
		e.DataDir = mnt
		e.configure = func(c *engine.Config) { c.MinFreeBytes = 64 << 20; c.DiskReserve = 256 << 20 }
		e.start()
		defer func() { e.configure = nil }()

		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "busy", Limits: client.Limits{DiskMB: 1024}})
		e.Must(err)
		e.Write(ws.ID, "kept.txt", "written before the volume filled")
		e.Save(ws.ID)

		// something else fills the volume
		filler := filepath.Join(mnt, "filler")
		run(e, "sh", "-c", "dd if=/dev/zero of="+filler+" bs=1M 2>/dev/null; sync; true")

		// new work is refused cleanly...
		_, err = e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		e.ErrCode(err, "insufficient_storage")
		_, err = e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "refused"})
		e.ErrCode(err, "insufficient_storage")
		// ...a write through the API fails without a trace...
		err = e.C.WriteFile(e.Ctx, ws.ID, "big.bin", make([]byte, 64<<20))
		if err == nil {
			e.T.Fatal("a 64 MB write succeeded on a full volume")
		}
		// ...and a program writing inside the workspace fails, or (once the
		// workspace's disk has hit write errors) the engine says why up front
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sh", "-c", "dd if=/dev/zero of=inside.bin bs=1M count=64 conv=fsync"}, TimeoutMS: 10_000})
		if err != nil {
			e.ErrCode(err, "insufficient_storage")
		} else if res.ExitCode == 0 && !res.TimedOut {
			e.T.Fatal("a 64 MB write inside the workspace succeeded on a full volume")
		}

		// room again: the workspace is intact and everything works
		e.Must(os.Remove(filler))
		run(e, "sync")
		time.Sleep(time.Second)
		e.Sh(ws.ID, "rm -f inside.bin big.bin")
		Equal(e, "earlier file", e.Read(ws.ID, "kept.txt"), "written before the volume filled")
		e.Write(ws.ID, "after.txt", "room again")
		e.Sh(ws.ID, "dd if=/dev/zero of=inside.bin bs=1M count=64 conv=fsync status=none && rm inside.bin")
		sv := e.Save(ws.ID)
		e.Resume(sv.ID)
		Equal(e, "after resume", e.Read(ws.ID, "after.txt"), "room again")
		e.Workspace("after-the-fill")
		entries, _ := os.ReadDir(filepath.Join(e.DataDir, "saves"))
		for _, d := range entries {
			if strings.HasPrefix(d.Name(), ".tmp-") {
				e.T.Fatalf("a refused save left %s on disk", d.Name())
			}
		}
	})
}

func run(e *Env, name string, args ...string) {
	e.T.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		e.T.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}

}

func init() {
	register("G6.4.4", func(e *Env) {
		if _, err := exec.LookPath("fallocate"); err != nil {
			e.T.Skip("needs fallocate")
		}
		vol := e.T.TempDir()
		img, mnt := filepath.Join(vol, "volume.img"), filepath.Join(vol, "data")
		e.Must(os.Mkdir(mnt, 0o700))
		run(e, "truncate", "-s", fmt.Sprintf("%dM", fullVolumeMB), img)
		run(e, "mkfs.ext4", "-q", "-F", "-m", "0", img)
		run(e, "mount", "-o", "loop", img, mnt)
		e.T.Cleanup(func() {
			unmountAll(mnt)
			_ = exec.Command("umount", "-l", mnt).Run()
		})
		e.stop()
		orig := e.DataDir
		e.T.Cleanup(func() { unmountAll(orig) })
		e.DataDir = mnt
		const reserve = 300 << 20
		e.configure = func(c *engine.Config) { c.MinFreeBytes = 64 << 20; c.DiskReserve = reserve }
		e.start()
		defer func() { e.configure = nil }()

		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "pressed", Limits: client.Limits{DiskMB: 1024}})
		e.Must(err)
		e.Write(ws.ID, "kept.txt", "written before the volume filled")
		e.Sh(ws.ID, "head -c 1048576 /dev/urandom > before.bin && sync")
		sum := e.Sh(ws.ID, "cksum before.bin")

		// something else takes the volume down to well under the reserve
		var st syscall.Statfs_t
		e.Must(syscall.Statfs(mnt, &st))
		free := int64(st.Bavail) * int64(st.Bsize) //nolint:unconvert
		filler := filepath.Join(mnt, "filler")
		run(e, "fallocate", "-l", fmt.Sprint(free-reserve/2), filler)

		deadline := time.Now().Add(5 * time.Second)
		for {
			err := e.C.WriteFile(e.Ctx, ws.ID, "probe.txt", []byte("x"))
			if client.IsCode(err, "insufficient_storage") {
				break
			}
			if time.Now().After(deadline) {
				e.T.Fatalf("an API write 5 s after the volume went under the reserve: %v (want insufficient_storage)", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		// a program's write waits for room (the filesystem is frozen) or fails;
		// it never completes onto a nearly full volume
		_, err = e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sh", "-c", "head -c 8388608 /dev/urandom > inside.bin && sync"}, TimeoutMS: 3_000})
		e.ErrCode(err, "insufficient_storage")
		Equal(e, "earlier file readable", e.Read(ws.ID, "kept.txt"), "written before the volume filled")
		_, err = e.C.Run(e.Ctx, ws.ID, "true")
		e.ErrCode(err, "insufficient_storage") // a new command would wait for room: refused at once

		// room again
		e.Must(os.Remove(filler))
		deadline = time.Now().Add(5 * time.Second)
		for {
			err := e.C.WriteFile(e.Ctx, ws.ID, "after.txt", []byte("room again"))
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				e.T.Fatalf("writes still refused 5 s after room came back: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		e.Sh(ws.ID, "head -c 8388608 /dev/urandom > inside.bin && sync")
		sv := e.Save(ws.ID)
		e.Resume(sv.ID)
		Equal(e, "after resume", e.Read(ws.ID, "after.txt"), "room again")
		Equal(e, "file from before the pressure", e.Sh(ws.ID, "cksum before.bin"), sum)
	})
}

// pressuredPair runs the engine on a small volume of its own, makes two
// workspaces (one, gone, with a process that keeps writing) and fills the
// volume until the guard freezes their disks.
func pressuredPair(e *Env) (mnt string, kept, gone *client.Workspace) {
	e.T.Helper()
	if _, err := exec.LookPath("fallocate"); err != nil {
		e.T.Skip("needs fallocate")
	}
	vol := e.T.TempDir()
	img := filepath.Join(vol, "volume.img")
	mnt = filepath.Join(vol, "data")
	e.Must(os.Mkdir(mnt, 0o700))
	run(e, "truncate", "-s", fmt.Sprintf("%dM", fullVolumeMB), img)
	run(e, "mkfs.ext4", "-q", "-F", "-m", "0", img)
	run(e, "mount", "-o", "loop", img, mnt)
	e.T.Cleanup(func() {
		unmountAll(mnt)
		_ = exec.Command("umount", "-l", mnt).Run()
	})
	e.stop()
	orig := e.DataDir
	e.T.Cleanup(func() { unmountAll(orig) })
	e.DataDir = mnt
	const reserve = 300 << 20
	e.configure = func(c *engine.Config) { c.MinFreeBytes = 64 << 20; c.DiskReserve = reserve }
	e.start()
	e.T.Cleanup(func() { e.configure = nil })

	var err error
	kept, err = e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "kept", Limits: client.Limits{DiskMB: 256}})
	e.Must(err)
	gone, err = e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "deleted", Limits: client.Limits{DiskMB: 256}})
	e.Must(err)
	// something in the workspace to be deleted keeps writing: once its disk
	// freezes, that writer waits in the kernel
	e.Sh(gone.ID, "nohup sh -c 'while :; do dd if=/dev/zero of=w.bin bs=1M count=1 conv=fsync 2>/dev/null; done' >/dev/null 2>&1 &")
	// whatever happens, never leave a frozen filesystem behind (cleanups run
	// last-registered first: before the unmounts above)
	e.T.Cleanup(func() {
		for _, id := range []string{kept.ID, gone.ID} {
			_ = exec.Command("fsfreeze", "-u", filepath.Join(mnt, "workspaces", id, "rootfs")).Run()
		}
	})
	var st syscall.Statfs_t
	e.Must(syscall.Statfs(mnt, &st))
	free := int64(st.Bavail) * int64(st.Bsize) //nolint:unconvert
	run(e, "fallocate", "-l", fmt.Sprint(free-reserve/2), filepath.Join(mnt, "filler"))
	deadline := time.Now().Add(5 * time.Second)
	for !client.IsCode(e.C.WriteFile(e.Ctx, kept.ID, "probe.txt", []byte("x")), "insufficient_storage") {
		if time.Now().After(deadline) {
			e.T.Fatal("the guard did not trip 5 s after the volume went under the reserve")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return mnt, kept, gone
}

func init() {
	register("G6.4.5", func(e *Env) {
		mnt, kept, gone := pressuredPair(e)
		// deleting a frozen workspace completes
		done := make(chan error, 1)
		go func() { done <- e.C.DeleteWorkspace(e.Ctx, gone.ID) }()
		select {
		case err := <-done:
			e.Must(err)
		case <-time.After(30 * time.Second):
			e.T.Fatal("deleting a workspace whose disk is frozen did not finish in 30 s")
		}

		// the engine stops with the volume still nearly full: nothing stays frozen
		e.stop()
		probe := filepath.Join(mnt, "workspaces", kept.ID, "rootfs", "after-stop.txt")
		wrote := make(chan error, 1)
		go func() { wrote <- os.WriteFile(probe, []byte("x"), 0o644) }()
		select {
		case <-wrote: // written, or failed for lack of room: either way not blocked
		case <-time.After(10 * time.Second):
			e.T.Fatal("a write to a workspace's disk blocks after the engine stopped: its filesystem was left frozen")
		}
		e.Must(os.Remove(filepath.Join(mnt, "filler")))
		e.start()
		Equal(e, "workspace after the pressure", e.Read(kept.ID, "after-stop.txt"), "x")
	})

	register("G6.4.6", func(e *Env) {
		mnt, kept, gone := pressuredPair(e)
		// a second between the delete's thaw and its sandbox stop: the guard
		// checks four times in it (a loaded host takes that long anyway)
		e.Failpoints.Enable(failpoint.DeleteAfterThaw, failpoint.Delay(time.Second))
		defer e.Failpoints.Disable(failpoint.DeleteAfterThaw)
		done := make(chan error, 1)
		go func() { done <- e.C.DeleteWorkspace(e.Ctx, gone.ID) }()
		select {
		case err := <-done:
			e.Must(err)
		case <-time.After(30 * time.Second):
			e.T.Fatal("deleting a workspace under disk pressure did not finish in 30 s: its disk was frozen again while it was stopped")
		}
		if _, err := e.C.GetWorkspace(e.Ctx, gone.ID); !client.IsCode(err, "not_found") {
			e.T.Fatalf("deleted workspace: %v", err)
		}
		// the guard still holds the other one
		e.ErrCode(e.C.WriteFile(e.Ctx, kept.ID, "probe.txt", []byte("x")), "insufficient_storage")
		e.Must(os.Remove(filepath.Join(mnt, "filler")))
	})
}
