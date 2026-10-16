package acceptance

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/objstore"
	"github.com/AQLD/cairn/internal/objstore/s3test"
)

// G6.5 and G7.7: saves in object storage. The engine runs against an
// in-memory S3 (internal/objstore/s3test) that can drop connections; the S3
// client itself is also tested against a real S3-compatible server.

// withObjectStore restarts the engine with saves copied to a fresh store.
func withObjectStore(e *Env) *s3test.Server {
	e.T.Helper()
	srv := s3test.New()
	e.T.Cleanup(srv.Close)
	ep, _ := url.Parse(srv.URL)
	s3 := &objstore.S3{Endpoint: ep, Region: "us-east-1", Bucket: "saves", AccessKey: "k", SecretKey: "s", Attempts: 4}
	store := objstore.WithPrefix(s3, "cluster")
	e.RestartWith(func(c *engine.Config) { c.Objects = store })
	return srv
}

func manifestKey(id string) string { return "saves/cluster/saves/" + id + "/manifest.json" }

func init() {
	register("G6.5.1", func(e *Env) {
		srv := withObjectStore(e)
		ws := e.Workspace("uploads")
		e.Write(ws.ID, "work.txt", "keep me")

		// a flaky connection: every third request is cut off
		var n atomic.Int32
		srv.Fail = func(*http.Request) bool { return n.Add(1)%3 == 0 }
		sv := e.Save(ws.ID)
		e.Must(e.Engine.SyncRemote(e.Ctx))
		got, err := e.C.GetSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "uploaded despite dropped connections", got.Uploaded, true)

		// the store is down: saving still works here, nothing half-uploaded
		// ever looks complete, and the upload happens once it is back
		srv.Fail = func(*http.Request) bool { return true }
		later := e.Save(ws.ID)
		if err := e.Engine.SyncRemote(e.Ctx); err == nil {
			e.T.Fatal("syncing to a store that is down reported success")
		}
		if _, ok := srv.Object(manifestKey(later.ID)); ok {
			e.T.Fatal("a save that failed to upload has a manifest in the store")
		}
		got, err = e.C.GetSave(e.Ctx, later.ID)
		e.Must(err)
		Equal(e, "save is ready here", string(got.State), "ready")
		Equal(e, "not marked uploaded", got.Uploaded, false)
		srv.Fail = nil
		e.Must(e.Engine.SyncRemote(e.Ctx))
		got, err = e.C.GetSave(e.Ctx, later.ID)
		e.Must(err)
		Equal(e, "uploaded once the store is back", got.Uploaded, true)
	})

	register("G6.5.2", func(e *Env) {
		srv := withObjectStore(e)
		ws := e.Workspace("verified")
		e.Write(ws.ID, "work.txt", "the real content")
		sv := e.Save(ws.ID)
		e.Must(e.Engine.SyncRemote(e.Ctx))
		key := "saves/cluster/saves/" + sv.ID + "/rootfs.tar"
		good, ok := srv.Object(key)
		if !ok {
			e.T.Fatalf("no %s in the store: %v", key, srv.Keys())
		}

		// the local copy is gone and the stored one is damaged: never used
		e.Must(os.RemoveAll(e.Engine.SaveDir(sv.ID)))
		bad := append([]byte{}, good...)
		bad[len(bad)/2] ^= 0xff
		srv.SetObject(key, bad)
		e.Write(ws.ID, "work.txt", "changed since")
		_, err := e.C.Resume(e.Ctx, sv.ID)
		e.ErrCode(err, "save_corrupt")
		if _, err := os.Stat(e.Engine.SaveDir(sv.ID)); err == nil {
			e.T.Fatal("a download that failed verification was put in place")
		}
		Equal(e, "workspace untouched", e.Read(ws.ID, "work.txt"), "changed since")

		// repaired in the store: the download verifies and the resume works
		srv.SetObject(key, good)
		e.Resume(sv.ID)
		Equal(e, "content after resume", e.Read(ws.ID, "work.txt"), "the real content")

		// damaged here, intact in the store: fetched again
		damageFile(e, filepath.Join(e.Engine.SaveDir(sv.ID), "rootfs.tar"))
		e.Write(ws.ID, "work.txt", "changed again")
		e.Resume(sv.ID)
		Equal(e, "content after self-repair", e.Read(ws.ID, "work.txt"), "the real content")
	})

	register("G7.7", func(e *Env) {
		withObjectStore(e)
		ws := e.Workspace("on-the-lost-server")
		e.Write(ws.ID, "work.txt", "made on the first server")
		quick := e.Save(ws.ID)
		var full *client.Save
		var pid int
		if e.Info.Capabilities.FullSave {
			background(e, ws.ID, counterProgram)
			pid = running(e, ws.ID).PID
			sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
			e.Must(err)
			full = sv
		}
		e.Must(e.Engine.SyncRemote(e.Ctx))

		// the server is lost: its disk with it. A new one, same bucket.
		_ = e.Engine.StopAll(e.Ctx)
		e.stop()
		lost := e.DataDir
		e.T.Cleanup(func() { unmountAll(lost) })
		e.DataDir = e.T.TempDir()
		e.start()

		saves, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		if len(saves) < 1 {
			e.T.Fatal("the new server does not know the lost server's saves")
		}
		copies, err := e.C.Copy(e.Ctx, quick.ID, 1)
		e.Must(err)
		Equal(e, "files carried over", e.Read(copies[0].ID, "work.txt"), "made on the first server")
		if full != nil {
			copies, err := e.C.Copy(e.Ctx, full.ID, 1)
			e.Must(err)
			Equal(e, "the process carries on elsewhere", running(e, copies[0].ID).PID, pid)
		}
	})
}
