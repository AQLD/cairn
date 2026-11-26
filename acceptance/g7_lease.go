package acceptance

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/api"
	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/objstore"
	"github.com/AQLD/cairn/internal/objstore/s3test"
)

// testLease is short, so lease scenarios run in seconds.
const testLease = 2 * time.Second

// leasedStore restarts the engine with saves (and owner records) in a fresh
// bucket and a short lease.
func leasedStore(e *Env) *s3test.Server {
	e.T.Helper()
	srv := s3test.New()
	e.T.Cleanup(srv.Close)
	e.RestartWith(func(c *engine.Config) { c.Objects = bucketAt(srv); c.LeaseTTL = testLease })
	return srv
}

func bucketAt(srv *s3test.Server) objstore.Store { return bucketAs(srv, "k") }

// bucketAs is the bucket reached with its own access key, so a test can cut
// one engine off (partition) by dropping only its requests.
func bucketAs(srv *s3test.Server, key string) objstore.Store {
	ep, _ := url.Parse(srv.URL)
	return objstore.WithPrefix(&objstore.S3{Endpoint: ep, Region: "us-east-1", Bucket: "saves", AccessKey: key, SecretKey: "s", Attempts: 2}, "cluster")
}

// partition drops every request made with access key (nil heals).
func partition(srv *s3test.Server, key string) {
	if key == "" {
		srv.SetFail(nil)
		return
	}
	srv.SetFail(func(r *http.Request) bool {
		return strings.Contains(r.Header.Get("Authorization"), "Credential="+key+"/")
	})
}

var peerKeys atomic.Int32

// peer is another server on the same bucket: its own disk, clock and API.
type peer struct {
	eng   *engine.Engine
	c     *client.Client
	clock *Clock
}

func startPeer(e *Env, srv *s3test.Server) *peer {
	e.T.Helper()
	p := &peer{clock: &Clock{}}
	cfg := engine.Config{DataDir: e.T.TempDir(), Runtime: e.rt, Now: p.clock.Now, Objects: bucketAs(srv, fmt.Sprintf("peer%d", peerKeys.Add(1))), LeaseTTL: testLease,
		NoDiskImages: os.Getenv("CAIRN_TEST_DISK_IMAGES") == "0"}
	if os.Getenv("CAIRN_TEST_TEMPLATE") != "" {
		cfg.DefaultTemplate = "base"
	}
	eng, err := engine.Open(e.Ctx, cfg)
	e.Must(err)
	if tmpl := os.Getenv("CAIRN_TEST_TEMPLATE"); tmpl != "" {
		f, err := os.Open(tmpl)
		e.Must(err)
		_, err = eng.ImportTemplate(e.Ctx, "base", f)
		f.Close()
		e.Must(err)
	}
	hs := httptest.NewServer(api.New(eng, slog.New(slog.DiscardHandler), ""))
	e.T.Cleanup(func() {
		_ = eng.StopAll(e.Ctx)
		hs.Close()
		_ = eng.Close()
		unmountAll(cfg.DataDir)
	})
	e.Must(eng.SyncRemote(e.Ctx)) // learn the bucket's saves
	p.eng, p.c = eng, client.New(hs.URL, "")
	return p
}

// owner is the workspace's owner record in the bucket.
func ownerRecord(e *Env, srv *s3test.Server, ws string) (engineID string, epoch int) {
	e.T.Helper()
	b, ok := srv.Object("saves/cluster/workspaces/" + ws + "/owner.json")
	if !ok {
		e.T.Fatalf("workspace %s has no owner record", ws)
	}
	var rec struct {
		Engine string `json:"engine"`
		Epoch  int    `json:"epoch"`
	}
	e.Must(json.Unmarshal(b, &rec))
	return rec.Engine, rec.Epoch
}

// leasedWorkspace makes a saved workspace on this engine, its save and owner
// record in the bucket.
func leasedWorkspace(e *Env) (*s3test.Server, *client.Workspace, *client.Save) {
	e.T.Helper()
	srv := leasedStore(e)
	ws := e.Workspace("leased")
	e.Write(ws.ID, "work.txt", "made here")
	sv := e.Save(ws.ID)
	e.Must(e.Engine.SyncRemote(e.Ctx))
	return srv, ws, sv
}

// loseServer stops this engine for good (its renewals with it).
func loseServer(e *Env) {
	e.T.Helper()
	_ = e.Engine.StopAll(e.Ctx)
	e.stop()
	lost := e.DataDir
	e.T.Cleanup(func() { unmountAll(lost) })
}

func init() {
	register("G7.8.1", func(e *Env) {
		srv, ws, sv := leasedWorkspace(e)
		first, _ := ownerRecord(e, srv, ws.ID)
		b := startPeer(e, srv)
		_, err := b.c.ResumeWith(e.Ctx, sv.ID, client.ResumeOptions{Adopt: true})
		e.ErrCode(err, "conflict")
		if !strings.Contains(err.Error(), first) {
			e.T.Fatalf("refusal %q: want it to name the owning engine %s", err, first)
		}
		// well past one lease: the owner keeps renewing, so still refused
		time.Sleep(3 * testLease)
		_, err = b.c.ResumeWith(e.Ctx, sv.ID, client.ResumeOptions{Adopt: true})
		e.ErrCode(err, "conflict")
		list, err := b.c.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "workspaces on the other server", len(list), 0)
		Equal(e, "still here", e.Read(ws.ID, "work.txt"), "made here")
	})

	register("G7.8.2", func(e *Env) {
		srv, ws, sv := leasedWorkspace(e)
		first, epoch := ownerRecord(e, srv, ws.ID)
		loseServer(e)
		b := startPeer(e, srv)
		b.clock.Advance(2 * testLease) // the lost server's lease has run out
		got, err := b.c.ResumeWith(e.Ctx, sv.ID, client.ResumeOptions{Adopt: true})
		e.Must(err)
		Equal(e, "adopted under its own ID", got.ID, ws.ID)
		now, epoch2 := ownerRecord(e, srv, ws.ID)
		if now == first || epoch2 <= epoch {
			e.T.Fatalf("owner record after adopting: engine %s epoch %d (was %s, %d): want the new engine and a higher epoch", now, epoch2, first, epoch)
		}
	})

	register("G7.8.3", func(e *Env) {
		srv, ws, sv := leasedWorkspace(e)
		loseServer(e)
		peers := []*peer{startPeer(e, srv), startPeer(e, srv)}
		errs := make([]error, len(peers))
		var wg sync.WaitGroup
		for i, p := range peers {
			p.clock.Advance(2 * testLease)
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = p.c.ResumeWith(e.Ctx, sv.ID, client.ResumeOptions{Adopt: true})
			}()
		}
		wg.Wait()
		won := 0
		for i, p := range peers {
			list, err := p.c.ListWorkspaces(e.Ctx)
			e.Must(err)
			if errs[i] == nil {
				won++
				if len(list) != 1 || list[0].ID != ws.ID {
					e.T.Fatalf("the winner's workspaces: %+v", list)
				}
			} else {
				e.ErrCode(errs[i], "conflict")
				Equal(e, "workspaces on the server that lost the race", len(list), 0)
			}
		}
		Equal(e, "servers that got the workspace", won, 1)
	})

	register("G7.8.4", func(e *Env) {
		srv, ws, sv := leasedWorkspace(e)
		partition(srv, "k") // this server is cut off from the bucket
		b := startPeer(e, srv)
		b.clock.Advance(2 * testLease) // its lease has run out, as seen from b
		_, err := b.c.ResumeWith(e.Ctx, sv.ID, client.ResumeOptions{Adopt: true})
		e.Must(err)
		newOwner, _ := ownerRecord(e, srv, ws.ID)

		partition(srv, "") // back: its next renewal finds the new owner
		e.Must(e.Engine.SyncRemote(e.Ctx))
		got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "state here", got.State, "moved")
		_, err = e.C.Exec(e.Ctx, ws.ID, client.Exec{Command: []string{"echo", "hi"}})
		e.ErrCode(err, "conflict")
		if !strings.Contains(err.Error(), newOwner) {
			e.T.Fatalf("refusal %q: want it to name the new engine %s", err, newOwner)
		}
		_, err = e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		e.ErrCode(err, "conflict")
		// renewals here leave the new owner's record alone
		e.Must(e.Engine.SyncRemote(e.Ctx))
		if now, _ := ownerRecord(e, srv, ws.ID); now != newOwner {
			e.T.Fatalf("owner record after the old server renewed: %s, want %s", now, newOwner)
		}
		// only deleting is left here, and it leaves the new owner's record
		e.Must(e.C.DeleteWorkspace(e.Ctx, ws.ID))
		if now, _ := ownerRecord(e, srv, ws.ID); now != newOwner {
			e.T.Fatalf("owner record after deleting the moved workspace here: %s, want %s", now, newOwner)
		}
		Equal(e, "still running on the new server", readOn(e, b, ws.ID, "work.txt"), "made here")
	})

	register("G7.8.5", func(e *Env) {
		srv, ws, sv := leasedWorkspace(e)
		partition(srv, "k")
		b := startPeer(e, srv)
		b.clock.Advance(2 * testLease)
		_, err := b.c.ResumeWith(e.Ctx, sv.ID, client.ResumeOptions{Adopt: true})
		e.Must(err)
		// cut off, this server does not know yet: it saves new work
		e.Write(ws.ID, "work.txt", "made after the takeover")
		late := e.Save(ws.ID)

		partition(srv, "")
		e.Must(e.Engine.SyncRemote(e.Ctx))
		if _, ok := srv.Object(manifestKey(late.ID)); ok {
			e.T.Fatal("a save made after another engine took the workspace reached the bucket")
		}
		// the save from before the takeover is still there
		if _, ok := srv.Object(manifestKey(sv.ID)); !ok {
			e.T.Fatal("the save made while this server owned the workspace is gone from the bucket")
		}
		e.Must(b.eng.SyncRemote(e.Ctx))
		saves, err := b.c.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		for _, s := range saves {
			if s.ID == late.ID {
				e.T.Fatal("the new owner sees the fenced server's late save")
			}
		}
		Equal(e, "the new owner's files", readOn(e, b, ws.ID, "work.txt"), "made here")
	})
}

func readOn(e *Env, p *peer, ws, path string) string {
	e.T.Helper()
	b, err := p.c.ReadFile(e.Ctx, ws, path)
	e.Must(err)
	return string(b)
}
