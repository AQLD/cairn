package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
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
		e.configure = func(c *engine.Config) { c.MinFreeBytes = 64 << 20 }
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
		res, err := e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"sh", "-c", "dd if=/dev/zero of=inside.bin bs=1M count=64 conv=fsync"}, TimeoutMS: 60_000})
		if err != nil {
			e.ErrCode(err, "insufficient_storage")
		} else if res.ExitCode == 0 {
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
