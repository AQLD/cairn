package acceptance

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/failpoint"
	"github.com/AQLD/cairn/internal/save"
	"github.com/AQLD/cairn/spec"
)

// G6.12: saves of a workspace made from a template hold only what changed
// since the template (a delta against it, the "base").

const deltaDir = "opt/cairn-delta"

// bigTemplateFile is large next to anything the scenarios change.
const bigTemplateFile = 8 << 20

// deltaTemplate imports a template: the suite's own root filesystem where it
// has one (a real Linux, for gVisor), plus a known tree under opt/cairn-delta
// with a big file, a hard link, a symlink and a folder to delete.
func deltaTemplate(e *Env, name string) *client.Template {
	e.T.Helper()
	return deltaTemplateWith(e, name, 0)
}

// deltaTemplateWith is deltaTemplate plus a file of extra bytes (opt/extra.bin)
// that nothing changes, to make the template's size stand out.
func deltaTemplateWith(e *Env, name string, extra int) *client.Template {
	e.T.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if p := os.Getenv("CAIRN_TEST_TEMPLATE"); p != "" {
		f, err := os.Open(p)
		e.Must(err)
		tr := tar.NewReader(f)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			e.Must(err)
			if strings.HasPrefix(strings.TrimPrefix(hdr.Name, "./"), deltaDir) {
				continue
			}
			e.Must(tw.WriteHeader(hdr))
			_, err = io.Copy(tw, tr)
			e.Must(err)
		}
		f.Close()
	}
	when := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	dir := func(name string) {
		e.Must(tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: when}))
	}
	file := func(name string, data []byte, mode int64) {
		e.Must(tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data)), ModTime: when}))
		_, err := tw.Write(data)
		e.Must(err)
	}
	big := make([]byte, bigTemplateFile)
	for i := range big {
		big[i] = byte(i*7 + i/4096)
	}
	for _, d := range []string{"opt", deltaDir, deltaDir + "/lib", deltaDir + "/gone", deltaDir + "/gone/x", deltaDir + "/keep"} {
		dir(d)
	}
	file(deltaDir+"/big.bin", big, 0o644)
	file(deltaDir+"/same.txt", []byte("AAAA"), 0o644)
	file(deltaDir+"/ref.txt", []byte("reference time"), 0o644)
	file(deltaDir+"/run.sh", []byte("#!/bin/sh\necho hi\n"), 0o644)
	for _, n := range []string{"a", "b", "c"} {
		file(deltaDir+"/lib/"+n+".txt", []byte("lib "+n+"\n"), 0o644)
	}
	file(deltaDir+"/gone/x/y.txt", []byte("to be deleted"), 0o644)
	file(deltaDir+"/keep/edit.txt", []byte("edited in place\n"), 0o644)
	e.Must(tw.WriteHeader(&tar.Header{Name: deltaDir + "/link", Typeflag: tar.TypeSymlink, Linkname: "lib/a.txt", ModTime: when}))
	e.Must(tw.WriteHeader(&tar.Header{Name: deltaDir + "/hard", Typeflag: tar.TypeLink, Linkname: deltaDir + "/lib/a.txt", ModTime: when}))
	if extra > 0 {
		data := make([]byte, extra)
		for i := range data {
			data[i] = byte(i*13 + i/8192)
		}
		file("opt/extra.bin", data, 0o644)
	}
	e.Must(tw.Close())
	t, err := e.C.ImportTemplate(e.Ctx, name, &buf)
	e.Must(err)
	return t
}

// agentEdits changes the workspace the ways agents do, including a same-size
// rewrite with the old modification time put back (only its ctime shows it).
const agentEdits = `cd ` + deltaDir + ` &&
printf BBBB > same.txt && touch -r ref.txt same.txt &&
rm -rf gone && echo new > lib/new.txt && chmod 755 run.sh &&
ln -sfn lib/b.txt link && echo more >> lib/c.txt &&
mkdir -p added/deep && echo n > added/deep/n.txt && rm lib/a.txt &&
echo more >> keep/edit.txt`

// keep/ is the case for parent folders' metadata: its only change is a file
// edited in place, which leaves the folder's time alone, while restoring the
// file (removed and written again) would not.

// fsListing is everything a restore must reproduce about a workspace's
// files, read from its filesystem on the host: per path its type, mode,
// owner, size, content hash, link target and modification time (to the
// second, as tar keeps it).
func fsListing(e *Env, ws string) map[string]string {
	e.T.Helper()
	root := filepath.Join(e.DataDir, "workspaces", ws, "rootfs")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." || rel == "lost+found" {
			if rel == "lost+found" && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		uid, gid := owner(info)
		line := fmt.Sprintf("%v %d:%d", info.Mode(), uid, gid)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			line += " -> " + l
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h := sha256.Sum256(data)
			line += fmt.Sprintf(" %d %s %d", len(data), hex.EncodeToString(h[:6]), info.ModTime().Round(time.Second).Unix())
		case info.IsDir():
			line += fmt.Sprintf(" %d", info.ModTime().Round(time.Second).Unix())
		default:
			return nil // not kept by saves (devices, fifos, sockets)
		}
		out[filepath.ToSlash(rel)] = line
		return nil
	})
	e.Must(err)
	return out
}

func owner(info fs.FileInfo) (uint32, uint32) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Uid, st.Gid
	}
	return 0, 0
}

func sameListing(e *Env, what string, got, want map[string]string) {
	e.T.Helper()
	var diffs []string
	for k, v := range want {
		if got[k] != v {
			diffs = append(diffs, fmt.Sprintf("  %s\n    want %q\n    got  %q", k, v, got[k]))
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("  %s\n    want (absent)\n    got  %q", k, v))
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		if len(diffs) > 20 {
			diffs = append(diffs[:20], fmt.Sprintf("  … and %d more", len(diffs)-20))
		}
		e.T.Fatalf("%s: %d paths differ\n%s", what, len(diffs), strings.Join(diffs, "\n"))
	}
}

// manifestOf reads a save's manifest from the engine's data folder.
func manifestOf(e *Env, id string) *save.Manifest {
	e.T.Helper()
	m, err := save.ReadManifest(e.Engine.SaveDir(id))
	e.Must(err)
	return m
}

func fileSize(m *save.Manifest, name string) int64 {
	for _, f := range m.Files {
		if f.Name == name {
			return f.Size
		}
	}
	return -1
}

func templatedWorkspace(e *Env, tmpl, name string) *client.Workspace {
	e.T.Helper()
	ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: name, Template: tmpl})
	e.Must(err)
	return ws
}

func init() {
	register("G6.12.1", func(e *Env) {
		e.Need(spec.NeedInProcess) // reads the workspace's files on the host
		t := deltaTemplate(e, "delta")
		ws := templatedWorkspace(e, "delta", "work")
		e.Sh(ws.ID, agentEdits)
		want := fsListing(e, ws.ID)

		sv := e.Save(ws.ID)
		if sv.Size >= 1<<20 || sv.Size <= 0 {
			e.T.Fatalf("save of a %d MB template after small edits is %d bytes: want only the changes", t.Size>>20, sv.Size)
		}
		e.T.Logf("template %d MB, save %d bytes", t.Size>>20, sv.Size)

		// a later mess, then back to the save: exactly what was saved
		e.Sh(ws.ID, `cd `+deltaDir+` && rm -rf lib && echo x > big.bin && echo junk > junk.txt`)
		e.Resume(sv.ID)
		sameListing(e, "workspace after resume", fsListing(e, ws.ID), want)

		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		sameListing(e, "copy of the save", fsListing(e, copies[0].ID), want)

		// a save of the copy is a delta too, and restores the same
		sv2 := e.Save(copies[0].ID)
		if sv2.Size >= 1<<20 {
			e.T.Fatalf("save of a copy is %d bytes: want only the changes", sv2.Size)
		}
		copies2, err := e.C.Copy(e.Ctx, sv2.ID, 1)
		e.Must(err)
		sameListing(e, "copy of the copy's save", fsListing(e, copies2[0].ID), want)
	})

	register("G6.12.2", func(e *Env) {
		deltaTemplate(e, "delta")
		ws := templatedWorkspace(e, "delta", "old")
		e.Sh(ws.ID, agentEdits)
		want := fsListing(e, ws.ID)

		// a save as engines made them before bases (format 1: the whole filesystem)
		e.RestartWith(func(c *engine.Config) { c.NoDeltaSaves = true })
		old := e.Save(ws.ID)
		m := manifestOf(e, old.ID)
		if m.FormatVersion != 1 || m.Base != nil || fileSize(m, save.RootfsFile) < bigTemplateFile {
			e.T.Fatalf("old-format save: version %d, base %v, rootfs %d bytes", m.FormatVersion, m.Base, fileSize(m, save.RootfsFile))
		}

		e.RestartWith(nil)
		e.Sh(ws.ID, `rm -rf `+deltaDir)
		e.Resume(old.ID)
		sameListing(e, "resumed from a format 1 save", fsListing(e, ws.ID), want)
		copies, err := e.C.Copy(e.Ctx, old.ID, 1)
		e.Must(err)
		sameListing(e, "copy of a format 1 save", fsListing(e, copies[0].ID), want)
	})

	register("G6.12.3", func(e *Env) {
		t := deltaTemplate(e, "delta")
		ws := templatedWorkspace(e, "delta", "orphan")
		e.Sh(ws.ID, agentEdits)
		want := fsListing(e, ws.ID)
		sv := e.Save(ws.ID)
		if m := manifestOf(e, sv.ID); m.Base == nil || m.Base.SHA256 != t.SHA256 {
			e.T.Fatalf("save's base %+v, want the template's %s", m.Base, t.SHA256)
		}

		// the template is replaced, then deleted: the save still restores
		var other bytes.Buffer
		tw := tar.NewWriter(&other)
		e.Must(tw.WriteHeader(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755}))
		e.Must(tw.Close())
		_, err := e.C.ImportTemplate(e.Ctx, "delta", &other)
		e.Must(err)
		e.Must(e.C.DeleteTemplate(e.Ctx, "delta"))
		e.Must(e.C.DeleteWorkspace(e.Ctx, ws.ID))
		basePath := filepath.Join(e.DataDir, "bases", t.SHA256+".tar")

		// kept while the save is, even past the purge (retention window)
		e.Clock.Advance(48 * time.Hour)
		_, err = e.Engine.PurgeExpired(e.Ctx)
		e.Must(err)
		if _, err := os.Stat(basePath); err != nil {
			e.T.Fatalf("base removed while a save needs it: %v", err)
		}
		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		sameListing(e, "copy after its template was replaced and deleted", fsListing(e, copies[0].ID), want)

		// nothing refers to it any more: purged with the last save
		e.Must(e.C.DeleteWorkspace(e.Ctx, copies[0].ID))
		e.Must(e.C.DeleteSave(e.Ctx, sv.ID))
		e.Clock.Advance(engine.DefaultRetention + 2*time.Hour)
		_, err = e.Engine.PurgeExpired(e.Ctx)
		e.Must(err)
		if _, err := os.Stat(basePath); err == nil {
			e.T.Fatal("base kept after nothing refers to it")
		}
	})

	register("G6.12.4", func(e *Env) {
		deltaTemplate(e, "delta")
		ws := templatedWorkspace(e, "delta", "full")
		e.Sh(ws.ID, agentEdits)
		e.Sh(ws.ID, `nohup sh -c 'n=0; while :; do n=$((n+1)); echo $$ $n > /tmp/counter; sleep 0.1; done' >/dev/null 2>&1 &`)
		time.Sleep(time.Second)
		before := e.Sh(ws.ID, "cat /tmp/counter")

		start := time.Now()
		sv, err := e.C.Save(e.Ctx, ws.ID, client.FullSave)
		e.Must(err)
		took := time.Since(start)
		m := manifestOf(e, sv.ID)
		fs := fileSize(m, save.DeltaFile)
		if m.Base == nil || fs < 0 || fs >= 1<<20 {
			e.T.Fatalf("full save: base %v, filesystem delta %d bytes; want a delta of the changes only", m.Base, fs)
		}
		e.T.Logf("full save in %s: %d bytes in all, filesystem delta %d bytes", took.Round(time.Millisecond), sv.Size, fs)

		e.Resume(sv.ID)
		time.Sleep(500 * time.Millisecond)
		after := e.Sh(ws.ID, "cat /tmp/counter")
		Equal(e, "same process after resume", strings.Fields(after)[0], strings.Fields(before)[0])
		Equal(e, "same-size edit kept", e.Sh(ws.ID, "cat "+deltaDir+"/same.txt"), "BBBB")
	})

	register("G6.12.5", func(e *Env) {
		t := deltaTemplate(e, "delta")
		ws := templatedWorkspace(e, "delta", "rot")
		e.Sh(ws.ID, agentEdits)
		sv := e.Save(ws.ID)
		// a restore that verifies it first, then damage that keeps its size and
		// time (bit rot): every restore must still hash every byte
		_, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		basePath := filepath.Join(e.DataDir, "bases", t.SHA256+".tar")
		info, err := os.Stat(basePath)
		e.Must(err)
		damageFile(e, basePath)
		e.Must(os.Chtimes(basePath, info.ModTime(), info.ModTime()))
		if after, _ := os.Stat(basePath); after.Size() != info.Size() {
			e.T.Fatal("damage changed the base's size")
		}

		_, err = e.C.Resume(e.Ctx, sv.ID)
		e.ErrCode(err, "save_corrupt")
		got, err := e.C.GetSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "save state", string(got.State), "corrupt")
		Equal(e, "workspace untouched", e.Sh(ws.ID, "cat "+deltaDir+"/same.txt"), "BBBB")
	})

	register("G6.12.6", func(e *Env) {
		srv := withObjectStore(e)
		t := deltaTemplate(e, "delta")
		ws := templatedWorkspace(e, "delta", "remote")
		e.Sh(ws.ID, agentEdits)
		want := fsListing(e, ws.ID)
		first := e.Save(ws.ID)
		e.Write(ws.ID, deltaDir+"/later.txt", "later")
		second := e.Save(ws.ID)
		e.Must(e.Engine.SyncRemote(e.Ctx))
		baseKey := "saves/cluster/bases/" + t.SHA256 + ".tar"
		if _, ok := srv.Object(baseKey); !ok {
			e.T.Fatalf("base not in object storage (%s)", baseKey)
		}
		if obj, _ := srv.Object(manifestKey(second.ID)); obj == nil {
			e.T.Fatal("second save not uploaded")
		}

		// this server loses its disk copies: the save and its base come back
		// from object storage, verified
		e.Must(os.RemoveAll(e.Engine.SaveDir(first.ID)))
		e.Must(os.Remove(filepath.Join(e.DataDir, "bases", t.SHA256+".tar")))
		copies, err := e.C.Copy(e.Ctx, first.ID, 1)
		e.Must(err)
		sameListing(e, "copy from object storage", fsListing(e, copies[0].ID), want)
		var m save.Manifest
		data, err := os.ReadFile(filepath.Join(e.Engine.SaveDir(first.ID), save.ManifestFile))
		e.Must(err)
		e.Must(json.Unmarshal(data, &m))
		Equal(e, "fetched save's base", m.Base.SHA256, t.SHA256)
	})

	// G7.14: a crash around a restore never makes a later save miss a change.
	register("G7.14", func(e *Env) {
		deltaTemplate(e, "delta")
		for _, point := range []string{failpoint.ResumeBeforeSwap, failpoint.RestoreBeforeBaseRecord} {
			ws := templatedWorkspace(e, "delta", "crash")
			sv := e.Save(ws.ID)
			// changes only a hash shows: same size, the template's modification time
			e.Sh(ws.ID, agentEdits)
			// long enough that a filesystem extracted after this would be
			// trusted past these edits (the engine's margin is 2 s): a record
			// written before the swap must not survive a crash
			time.Sleep(3 * time.Second)
			e.Failpoints.Enable(point, failpoint.ErrCrash)
			if _, err := e.C.Resume(e.Ctx, sv.ID); err == nil {
				e.T.Fatalf("%s: resume reported success despite the crash", point)
			}
			e.Restart()
			if point == failpoint.RestoreBeforeBaseRecord {
				// the restore completed: change it again the same way
				e.Sh(ws.ID, agentEdits)
			}
			want := fsListing(e, ws.ID)
			after := e.Save(ws.ID)
			copies, err := e.C.Copy(e.Ctx, after.ID, 1)
			e.Must(err)
			sameListing(e, point+": copy of a save made after the crash", fsListing(e, copies[0].ID), want)
		}
	})
}

func init() {
	// G6.12.7: a file a restore wrote is never trusted as base content by the
	// next save, however long the rest of the restore took.
	register("G6.12.7", func(e *Env) {
		deltaTemplate(e, "delta")
		ws := templatedWorkspace(e, "delta", "long-restore")
		// same.txt: a same-size rewrite keeping the base's time; then a big
		// tail after it in walk order, so restoring takes far longer than the
		// engine's change-time margin after same.txt is written
		e.Sh(ws.ID, `cd `+deltaDir+` && printf BBBB > same.txt && touch -r ref.txt same.txt &&
mkdir -p zz-bulk && for i in $(seq 1 64); do head -c 4194304 /dev/zero > zz-bulk/$i.bin; done`)
		sv := e.Save(ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		Equal(e, "same.txt in the restored copy", e.Sh(copies[0].ID, "cat "+deltaDir+"/same.txt"), "BBBB")

		// the copy, unchanged, saved again and restored again
		again := e.Save(copies[0].ID)
		copies2, err := e.C.Copy(e.Ctx, again.ID, 1)
		e.Must(err)
		Equal(e, "same.txt after a save of the restore", e.Sh(copies2[0].ID, "cat "+deltaDir+"/same.txt"), "BBBB")
	})
}

func init() {
	// G6.12.8: a base that cannot be fetched now is not a damaged save.
	register("G6.12.8", func(e *Env) {
		srv := withObjectStore(e)
		t := deltaTemplate(e, "delta")
		basePath := filepath.Join(e.DataDir, "bases", t.SHA256+".tar")
		ws := templatedWorkspace(e, "delta", "unreachable")
		e.Sh(ws.ID, agentEdits)
		sv := e.Save(ws.ID)
		e.Must(e.Engine.SyncRemote(e.Ctx))

		// the base is gone here and the store is down: refused, not corrupt
		e.Must(os.Remove(basePath))
		srv.SetFail(func(*http.Request) bool { return true })
		_, err := e.C.Resume(e.Ctx, sv.ID)
		e.ErrCode(err, "conflict")
		got, err := e.C.GetSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "save state while its base is unreachable", string(got.State), "ready")
		srv.SetFail(nil)
		e.Resume(sv.ID)
		Equal(e, "restored once the store is back", e.Sh(ws.ID, "cat "+deltaDir+"/same.txt"), "BBBB")

		// a save committed just before a crash, recovered while its base is
		// unreachable: kept (missing), restorable once the store is back
		e.Failpoints.Enable(failpoint.SaveAfterCommit, failpoint.ErrCrash)
		if _, err := e.C.Save(e.Ctx, ws.ID, client.QuickSave); err == nil {
			e.T.Fatal("save reported success despite the crash")
		}
		e.Must(os.Remove(basePath))
		srv.SetFail(func(*http.Request) bool { return true })
		e.Restart()
		saves, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		var pending *client.Save
		for i := range saves {
			if saves[i].ID != sv.ID {
				pending = &saves[i]
			}
		}
		if pending == nil {
			e.T.Fatalf("the save committed before the crash was discarded: %+v", saves)
		}
		Equal(e, "recovered save whose base is unreachable", string(pending.State), "missing")
		srv.SetFail(nil)
		fixed, err := e.C.VerifySave(e.Ctx, pending.ID)
		e.Must(err)
		Equal(e, "after the store is back", string(fixed.State), "ready")
		copies, err := e.C.Copy(e.Ctx, pending.ID, 1)
		e.Must(err)
		Equal(e, "copy of the recovered save", e.Sh(copies[0].ID, "cat "+deltaDir+"/same.txt"), "BBBB")
	})
}

func init() {
	// G6.12.9: a base's index is never trusted unchecked.
	register("G6.12.9", func(e *Env) {
		t := deltaTemplate(e, "delta")
		// the index claims the template's same.txt holds what the agent will
		// write; trusted, a save would skip the change
		indexPath := filepath.Join(e.DataDir, "bases", t.SHA256+".index.json")
		data, err := os.ReadFile(indexPath)
		e.Must(err)
		var idx save.Index
		e.Must(json.Unmarshal(data, &idx))
		entry := idx.Entries[deltaDir+"/same.txt"]
		sum := sha256.Sum256([]byte("BBBB"))
		entry.SHA256 = hex.EncodeToString(sum[:])
		idx.Entries[deltaDir+"/same.txt"] = entry
		data, err = json.Marshal(&idx)
		e.Must(err)
		e.Must(os.WriteFile(indexPath, data, 0o600))
		e.Restart() // read from disk again

		ws := templatedWorkspace(e, "delta", "tampered")
		e.Sh(ws.ID, `cd `+deltaDir+` && printf BBBB > same.txt && touch -r ref.txt same.txt`)
		sv := e.Save(ws.ID)
		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		Equal(e, "same.txt in a copy", e.Sh(copies[0].ID, "cat "+deltaDir+"/same.txt"), "BBBB")
	})

	register("G6.12.10", func(e *Env) {
		t := deltaTemplate(e, "grace")
		basePath := filepath.Join(e.DataDir, "bases", t.SHA256+".tar")
		e.Clock.Advance(3 * time.Hour) // the template is old

		ws := templatedWorkspace(e, "grace", "late") // uses the base now
		e.Must(e.C.DeleteTemplate(e.Ctx, "grace"))
		e.Must(e.C.DeleteWorkspace(e.Ctx, ws.ID))
		_, err := e.Engine.PurgeExpired(e.Ctx) // collects unused bases
		e.Must(err)
		if _, err := os.Stat(basePath); err != nil {
			e.T.Fatalf("base used minutes ago was collected: %v", err)
		}

		e.Clock.Advance(2 * time.Hour)
		_, err = e.Engine.PurgeExpired(e.Ctx)
		e.Must(err)
		if _, err := os.Stat(basePath); err == nil {
			e.T.Fatal("base kept long after its last use with nothing referring to it")
		}
	})
}
