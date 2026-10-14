package acceptance

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/failpoint"
	"github.com/AQLD/cairn/internal/save"
)

// G6: a save is never lost or silently broken.

func init() {
	register("G6.1.1", func(e *Env) {
		ws, sv := savedWorkspace(e)
		damage(e, sv.ID, func(path string) {
			data, err := os.ReadFile(path)
			e.Must(err)
			data[len(data)/2] ^= 0xFF
			e.Must(os.WriteFile(path, data, 0o600))
		})
		mustRefuseCorrupt(e, ws, sv)
	})

	register("G6.1.2", func(e *Env) {
		ws, sv := savedWorkspace(e)
		damage(e, sv.ID, func(path string) {
			info, err := os.Stat(path)
			e.Must(err)
			e.Must(os.Truncate(path, info.Size()/2))
		})
		mustRefuseCorrupt(e, ws, sv)
	})

	register("G6.1.3", func(e *Env) {
		ws, sv := savedWorkspace(e)
		e.Must(os.Remove(filepath.Join(e.Engine.SaveDir(sv.ID), save.ManifestFile)))
		mustRefuseCorrupt(e, ws, sv)
	})

	register("G6.1.4", func(e *Env) {
		ws, sv := savedWorkspace(e)
		editManifest(e, sv.ID, func(m *save.Manifest) { m.FormatVersion = save.FormatVersion + 1 })
		_, err := e.C.Resume(e.Ctx, sv.ID)
		if err == nil {
			e.T.Fatal("a save from a newer format was restored")
		}
		Equal(e, "workspace untouched", e.Read(ws.ID, "data.txt"), "precious")
	})

	register("G6.1.5", func(e *Env) {
		ws, sv := savedWorkspace(e)
		// replace the filesystem image with one that writes outside the workspace,
		// with checksums fixed up so only the path check can stop it
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		evil := []byte("escaped")
		e.Must(tw.WriteHeader(&tar.Header{Name: "../../escape.txt", Mode: 0o644, Size: int64(len(evil)), Typeflag: tar.TypeReg}))
		_, err := tw.Write(evil)
		e.Must(err)
		e.Must(tw.Close())
		dir := e.Engine.SaveDir(sv.ID)
		e.Must(os.WriteFile(filepath.Join(dir, save.RootfsFile), buf.Bytes(), 0o600))
		sum := sha256.Sum256(buf.Bytes())
		editManifest(e, sv.ID, func(m *save.Manifest) {
			for i := range m.Files {
				if m.Files[i].Name == save.RootfsFile {
					m.Files[i].Size = int64(buf.Len())
					m.Files[i].SHA256 = hex.EncodeToString(sum[:])
				}
			}
		})
		_, err = e.C.Resume(e.Ctx, sv.ID)
		e.ErrCode(err, "save_corrupt")
		if _, statErr := os.Stat(filepath.Join(e.DataDir, "workspaces", "escape.txt")); statErr == nil {
			e.T.Fatal("file escaped the workspace")
		}
		Equal(e, "workspace untouched", e.Read(ws.ID, "data.txt"), "precious")
	})

	register("G6.2", func(e *Env) {
		ws := e.Workspace("loop")
		for i := 0; i < Cycles(); i++ {
			want := "cycle " + strconv.Itoa(i)
			e.Write(ws.ID, "state.txt", want)
			e.Sh(ws.ID, "head -c 4096 /dev/urandom > noise.bin")
			sum := e.Sh(ws.ID, "cksum noise.bin")
			sv := e.Save(ws.ID)
			e.Write(ws.ID, "state.txt", "overwritten")
			e.Sh(ws.ID, "rm noise.bin")
			e.Resume(sv.ID)
			Equal(e, "state after resume", e.Read(ws.ID, "state.txt"), want)
			Equal(e, "binary after resume", e.Sh(ws.ID, "cksum noise.bin"), sum)
		}
	})

	register("G6.3", func(e *Env) {
		ws := e.Workspace("halfsave")
		e.Write(ws.ID, "a.txt", "x")
		e.Failpoints.Enable(failpoint.SaveBeforeCommit, failpoint.ErrCrash)
		_, err := e.C.Save(e.Ctx, ws.ID, client.QuickSave)
		if err == nil {
			e.T.Fatal("save succeeded despite the crash")
		}
		e.Restart()
		saves, err := e.C.ListSaves(e.Ctx, ws.ID)
		e.Must(err)
		Equal(e, "saves after crash", len(saves), 0)
		entries, err := os.ReadDir(filepath.Join(e.DataDir, "saves"))
		e.Must(err)
		Equal(e, "leftover save folders", len(entries), 0)
		// the workspace still works and can save normally
		Equal(e, "workspace intact", e.Read(ws.ID, "a.txt"), "x")
		e.Save(ws.ID)
	})

	register("G6.9", func(e *Env) {
		ws := e.Workspace("deleted")
		e.Write(ws.ID, "keep.txt", "survives")
		sv := e.Save(ws.ID)
		e.Must(e.C.DeleteWorkspace(e.Ctx, ws.ID))
		_, err := e.C.GetWorkspace(e.Ctx, ws.ID)
		e.ErrCode(err, "not_found")
		got, err := e.C.GetSave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "save state", got.State, "ready")
		_, err = e.C.Resume(e.Ctx, sv.ID)
		e.ErrCode(err, "conflict") // its workspace is gone: copy instead
		copies, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		Equal(e, "copy of deleted workspace's save", e.Read(copies[0].ID, "keep.txt"), "survives")
	})
}

func savedWorkspace(e *Env) (*client.Workspace, *client.Save) {
	e.T.Helper()
	ws := e.Workspace("victim")
	e.Write(ws.ID, "data.txt", "precious")
	e.Sh(ws.ID, "head -c 32768 /dev/urandom > bulk.bin")
	return ws, e.Save(ws.ID)
}

func damage(e *Env, saveID string, fn func(path string)) {
	e.T.Helper()
	fn(filepath.Join(e.Engine.SaveDir(saveID), save.RootfsFile))
}

func editManifest(e *Env, saveID string, fn func(*save.Manifest)) {
	e.T.Helper()
	path := filepath.Join(e.Engine.SaveDir(saveID), save.ManifestFile)
	data, err := os.ReadFile(path)
	e.Must(err)
	var m save.Manifest
	e.Must(json.Unmarshal(data, &m))
	fn(&m)
	data, err = json.Marshal(m)
	e.Must(err)
	e.Must(os.WriteFile(path, data, 0o600))
}

// mustRefuseCorrupt checks a damaged save is refused for both resume and copy,
// and the workspace is left exactly as it was.
func mustRefuseCorrupt(e *Env, ws *client.Workspace, sv *client.Save) {
	e.T.Helper()
	e.Write(ws.ID, "data.txt", "current")
	_, err := e.C.Resume(e.Ctx, sv.ID)
	e.ErrCode(err, "save_corrupt")
	_, err = e.C.Copy(e.Ctx, sv.ID, 1)
	e.ErrCode(err, "save_corrupt")
	Equal(e, "workspace untouched", e.Read(ws.ID, "data.txt"), "current")
	got, err := e.C.GetWorkspace(e.Ctx, ws.ID)
	e.Must(err)
	Equal(e, "workspace state", got.State, "running")
}

