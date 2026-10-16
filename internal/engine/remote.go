package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/internal/save"
	"github.com/AQLD/cairn/internal/store"
)

// Saves in object storage (Config.Objects). Every ready save is copied to
//
//	saves/<save id>/<file>   (the manifest last: its presence means complete)
//
// in the background, retried until it succeeds. A save that is missing here
// (another server made it, or this one lost its disk) is downloaded and
// verified against its checksums before anything uses it, and one that fails
// verification here is fetched again. A server starting on an empty disk
// learns the bucket's saves, so a lost server's workspaces can be copied and
// carried on elsewhere.

func remoteDir(id string) string               { return "saves/" + id + "/" }
func remoteKey(id, name string) string         { return remoteDir(id) + name }
func (e *Engine) remoteOn() bool               { return e.cfg.Objects != nil }
func (e *Engine) downloadDir(id string) string { return filepath.Join(e.savesDir(), ".tmp-dl-"+id) }

// kickUploads wakes the uploader (it also runs on a timer).
func (e *Engine) kickUploads() {
	if !e.remoteOn() {
		return
	}
	select {
	case e.uploadKick <- struct{}{}:
	default:
	}
}

// uploader copies pending saves to object storage until ctx ends.
func (e *Engine) uploader(ctx context.Context) {
	defer close(e.uploaderDone)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		e.uploadPending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-e.uploadKick:
		case <-tick.C:
		}
	}
}

func (e *Engine) uploadPending(ctx context.Context) {
	pending, err := e.store.PendingUploads(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Error("listing saves to upload", "err", err)
		}
		return
	}
	for _, sv := range pending {
		if ctx.Err() != nil {
			return
		}
		if err := e.upload(ctx, sv.ID); err != nil {
			if ctx.Err() == nil {
				e.log.Error("save upload failed; will retry", "save", sv.ID, "err", err)
			}
			return // the store is likely down: try again on the next round
		}
		e.log.Info("save uploaded", "save", sv.ID, "store", e.cfg.Objects.String())
	}
}

// upload copies one save to object storage: data files first, manifest last.
func (e *Engine) upload(ctx context.Context, id string) error {
	dir := e.saveDir(id)
	m, err := save.Verify(dir) // never upload a damaged save
	if errors.Is(err, os.ErrNotExist) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	for _, f := range m.Files {
		if err := e.putFile(ctx, id, dir, f.Name); err != nil {
			return err
		}
	}
	if err := e.putFile(ctx, id, dir, save.ManifestFile); err != nil {
		return err
	}
	return e.store.MarkUploaded(ctx, id)
}

func (e *Engine) putFile(ctx context.Context, id, dir, name string) error {
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return e.cfg.Objects.Put(ctx, remoteKey(id, name), f, info.Size())
}

// ensureLocal makes sure a save's files are here, downloading them if the
// save is in object storage.
func (e *Engine) ensureLocal(ctx context.Context, sv *store.Save) error {
	if _, err := os.Stat(filepath.Join(e.saveDir(sv.ID), save.ManifestFile)); err == nil {
		return nil
	}
	if !e.remoteOn() || !sv.Uploaded {
		return nil // the caller's own checks report it missing
	}
	return e.fetch(ctx, sv.ID)
}

// fetch downloads a save from object storage and puts it in place only if it
// verifies: a damaged or partial download is never used.
func (e *Engine) fetch(ctx context.Context, id string) error {
	if err := e.needSpace("downloading the save", 0); err != nil {
		return err
	}
	tmp := e.downloadDir(id)
	_ = os.RemoveAll(tmp)
	defer os.RemoveAll(tmp) // no-op once renamed
	keys, err := e.cfg.Objects.List(ctx, remoteDir(id))
	if err != nil {
		return fmt.Errorf("list save %s in %s: %w", id, e.cfg.Objects, err)
	}
	complete := false
	for _, k := range keys {
		name := strings.TrimPrefix(k, remoteDir(id))
		if name == save.ManifestFile {
			complete = true
		}
		local := filepath.Join(tmp, filepath.FromSlash(name))
		if !strings.HasPrefix(local, tmp+string(filepath.Separator)) || path.Clean(name) != name {
			return fmt.Errorf("%w: save %s holds an unsafe name %q", ErrCorrupt, id, name)
		}
		if err := e.getFile(ctx, k, local); err != nil {
			return fmt.Errorf("download save %s: %w", id, noSpace(err))
		}
	}
	if !complete {
		return fmt.Errorf("%w: save %s is not (completely) in %s", ErrNotFound, id, e.cfg.Objects)
	}
	if _, err := save.Verify(tmp); err != nil {
		return fmt.Errorf("downloaded save %s does not verify: %w", id, err)
	}
	_ = os.RemoveAll(e.saveDir(id)) // a damaged local copy being replaced
	if err := os.Rename(tmp, e.saveDir(id)); err != nil {
		return err
	}
	e.log.Info("save downloaded and verified", "save", id, "store", e.cfg.Objects.String())
	return syncDir(e.savesDir())
}

func (e *Engine) getFile(ctx context.Context, key, local string) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return err
	}
	rc, err := e.cfg.Objects.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(local, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, rc)
	if err == nil {
		err = fsync(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// deleteRemote removes a save's objects.
func (e *Engine) deleteRemote(ctx context.Context, id string) error {
	if !e.remoteOn() {
		return nil
	}
	keys, err := e.cfg.Objects.List(ctx, remoteDir(id))
	if err != nil {
		return err
	}
	// the manifest first: a half-deleted save is then incomplete, never used
	for _, k := range keys {
		if strings.HasSuffix(k, "/"+save.ManifestFile) {
			if err := e.cfg.Objects.Delete(ctx, k); err != nil {
				return err
			}
		}
	}
	for _, k := range keys {
		if !strings.HasSuffix(k, "/"+save.ManifestFile) {
			if err := e.cfg.Objects.Delete(ctx, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// importRemote records the bucket's complete saves this engine does not know,
// so a server that lost its disk (or a new one) can carry on from them.
func (e *Engine) importRemote(ctx context.Context) error {
	keys, err := e.cfg.Objects.List(ctx, "saves/")
	if err != nil {
		return err
	}
	n := 0
	for _, k := range keys {
		id, name, ok := strings.Cut(strings.TrimPrefix(k, "saves/"), "/")
		if !ok || name != save.ManifestFile {
			continue
		}
		if _, err := e.store.GetSave(ctx, id); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		m, err := e.remoteManifest(ctx, id)
		if err != nil {
			e.log.Error("save in object storage unreadable; skipped", "save", id, "err", err)
			continue
		}
		sv := store.Save{ID: id, WorkspaceID: m.WorkspaceID, Kind: string(m.Kind), State: store.SaveReady,
			Size: m.Size(), CreatedAt: m.CreatedAt, Uploaded: true}
		if err := e.store.CreateSave(ctx, sv); err != nil {
			return err
		}
		n++
	}
	if n > 0 {
		e.log.Info("learned saves from object storage", "count", n, "store", e.cfg.Objects.String())
	}
	return nil
}

func (e *Engine) remoteManifest(ctx context.Context, id string) (*save.Manifest, error) {
	rc, err := e.cfg.Objects.Get(ctx, remoteKey(id, save.ManifestFile))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var m save.Manifest
	if err := json.NewDecoder(io.LimitReader(rc, 16<<20)).Decode(&m); err != nil {
		return nil, err
	}
	if m.SaveID != id {
		return nil, fmt.Errorf("manifest names save %q", m.SaveID)
	}
	return &m, nil
}

// SyncRemote uploads what is pending now and learns the bucket's saves; for
// tests and for operators who want it done before shutting a server down.
func (e *Engine) SyncRemote(ctx context.Context) error {
	if !e.remoteOn() {
		return nil
	}
	pending, err := e.store.PendingUploads(ctx)
	if err != nil {
		return err
	}
	for _, sv := range pending {
		if err := e.upload(ctx, sv.ID); err != nil {
			return fmt.Errorf("upload save %s: %w", sv.ID, err)
		}
	}
	return e.importRemote(ctx)
}
