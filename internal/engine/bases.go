package engine

// Bases: the template tars that saves store their filesystem as a delta
// against (internal/save/delta.go).
//
//	bases/<sha256>.tar          the template's tar, named by its content
//	bases/<sha256>.index.json   every path in it, for computing deltas (rebuilt
//	                            from the tar whenever missing)
//	workspaces/<id>/rootfs.base which base a workspace's filesystem was
//	                            extracted from, and when (written only after
//	                            the filesystem is in place)
//
// A base is never changed and is kept while a template, a workspace or a save
// (deleted ones within retention included) refers to it, so a save restores
// after its template is replaced or deleted. In object storage a base is
// uploaded once, before the first save that needs it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AQLD/cairn/internal/save"
)

// baseGrace keeps a base nothing refers to for a while: a workspace may be
// being created from it this very moment.
const baseGrace = time.Hour

func (e *Engine) basesDir() string            { return filepath.Join(e.cfg.DataDir, "bases") }
func (e *Engine) basePath(sha string) string  { return filepath.Join(e.basesDir(), sha+".tar") }
func (e *Engine) indexPath(sha string) string { return filepath.Join(e.basesDir(), sha+".index.json") }
func (e *Engine) baseRecordPath(id string) string {
	return filepath.Join(e.workspaceDir(id), "rootfs.base")
}
func remoteBaseKey(sha string) string { return "bases/" + sha + ".tar" }

// baseState caches what is expensive to redo per save or restore.
type baseState struct {
	mu      sync.Mutex
	indexes map[string]*save.Index
}

func (b *baseState) init() {
	if b.indexes == nil {
		b.indexes = map[string]*save.Index{}
	}
}

// ensureBase makes sure base sha exists, linking (or copying) it from src, a
// tar whose content is sha. Atomic: built under a temporary name and renamed.
func (e *Engine) ensureBase(sha, src string) error {
	if _, err := os.Stat(e.basePath(sha)); err == nil {
		return nil
	}
	if err := os.MkdirAll(e.basesDir(), 0o700); err != nil {
		return err
	}
	tmp := e.basePath(sha) + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	defer os.Remove(tmp)
	if err := os.Link(src, tmp); err != nil {
		if err := copyFile(src, tmp); err != nil {
			return err
		}
	}
	got, _, err := hashFileSHA(tmp)
	if err != nil {
		return err
	}
	if got != sha {
		return fmt.Errorf("template tar changed while it was being kept as a base (%s, want %s)", got, sha)
	}
	if err := os.Rename(tmp, e.basePath(sha)); err != nil {
		return err
	}
	return syncDir(e.basesDir())
}

// baseIndex returns the index of base sha. The stored index is used only if
// it matches its own checksum (bases/<sha>.index.sha256); otherwise it is
// rebuilt from the base, whose bytes must match sha as they are read. A wrong
// index could make a save skip a change, so it is never trusted unchecked.
func (e *Engine) baseIndex(sha string) (*save.Index, error) {
	e.bases.mu.Lock()
	e.bases.init()
	idx := e.bases.indexes[sha]
	e.bases.mu.Unlock()
	if idx != nil {
		return idx, nil
	}
	if data, err := os.ReadFile(e.indexPath(sha)); err == nil {
		want, _ := os.ReadFile(e.indexPath(sha) + ".sha256")
		sum := sha256.Sum256(data)
		if strings.TrimSpace(string(want)) == hex.EncodeToString(sum[:]) {
			if idx, err = save.ReadIndex(bytes.NewReader(data)); err != nil {
				idx = nil
			}
		}
		if idx == nil {
			e.log.Warn("base index does not match its checksum; rebuilding it", "base", sha)
		}
	}
	if idx == nil {
		f, err := os.Open(e.basePath(sha))
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		tee := io.TeeReader(f, h)
		idx, err = save.BuildIndex(tee)
		if err == nil {
			_, err = io.Copy(io.Discard, tee) // the rest (tar padding), so the hash covers the file
		}
		f.Close()
		if err != nil {
			return nil, err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != sha {
			return nil, fmt.Errorf("%w: base %s does not match its checksum", save.ErrCorrupt, sha)
		}
		var buf bytes.Buffer
		if err := save.WriteIndex(&buf, idx); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(buf.Bytes())
		if err := writeAtomic(e.indexPath(sha), buf.Bytes()); err == nil {
			err = writeAtomic(e.indexPath(sha)+".sha256", []byte(hex.EncodeToString(sum[:])+"\n"))
			if err != nil {
				e.log.Warn("could not store base index checksum", "base", sha, "err", err)
			}
		} else {
			e.log.Warn("could not store base index", "base", sha, "err", err)
		}
	}
	e.bases.mu.Lock()
	e.bases.indexes[sha] = idx
	e.bases.mu.Unlock()
	return idx, nil
}

// writeAtomic writes data to path through a synced temporary file.
func writeAtomic(path string, data []byte) error {
	tmp := path + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// cleanupBases removes temporary files a crash left in bases/.
func (e *Engine) cleanupBases() {
	entries, err := os.ReadDir(e.basesDir())
	if err != nil {
		return
	}
	for _, d := range entries {
		if strings.Contains(d.Name(), ".tmp-") {
			_ = os.Remove(filepath.Join(e.basesDir(), d.Name()))
		}
	}
}

// errBaseMissing: a base is not here (and could not be fetched). It says
// nothing about the save's own files, so it must not mark a save corrupt: the
// base may come back (object storage reachable again, a backup restored).
var errBaseMissing = errors.New("base missing")

// verifyBase hashes every byte of a base. Every restore does this, as for
// the save's own files: nothing restores without passing verification.
func (e *Engine) verifyBase(ref *save.BaseRef) error {
	info, err := os.Stat(e.basePath(ref.SHA256))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", errBaseMissing, ref.SHA256)
	}
	if err != nil {
		return err
	}
	if info.Size() != ref.Size {
		return fmt.Errorf("%w: base %s is %d bytes, want %d", save.ErrCorrupt, ref.SHA256, info.Size(), ref.Size)
	}
	got, _, err := hashFileSHA(e.basePath(ref.SHA256))
	if err != nil {
		return err
	}
	if got != ref.SHA256 {
		return fmt.Errorf("%w: base %s does not match its checksum", save.ErrCorrupt, ref.SHA256)
	}
	return nil
}

// checkBase makes sure the base a save needs is here and intact, fetching it
// from object storage when it is missing or damaged here. seen, if set,
// holds bases already verified in this run (a scrub verifies each once).
// Only damage it could not repair is save.ErrCorrupt; a base that is missing
// and could not be fetched (the store unreachable) is errBaseMissing.
func (e *Engine) checkBase(ctx context.Context, m *save.Manifest, seen map[string]bool) error {
	if m.Base == nil || seen[m.Base.SHA256] {
		return nil
	}
	err := e.verifyBase(m.Base)
	if err != nil && e.remoteOn() {
		if ferr := e.fetchBase(ctx, m.Base); ferr != nil {
			e.log.Error("could not fetch base from object storage", "base", m.Base.SHA256, "err", ferr)
			if errors.Is(ferr, save.ErrCorrupt) {
				err = ferr // the store's copy is damaged too
			}
		} else {
			err = e.verifyBase(m.Base)
		}
	}
	if err == nil && seen != nil {
		seen[m.Base.SHA256] = true
	}
	return err
}

// baseRecord is which base a workspace's filesystem was extracted from, when
// extraction finished, and the margin before that time a file's change time
// must be to be trusted (save.ChangeTimeMargin: the filesystem's timestamp
// coarseness). A wider margin only costs hashing.
type baseRecord struct {
	Base      save.BaseRef  `json:"base"`
	Extracted time.Time     `json:"extracted"`
	Margin    time.Duration `json:"margin"`
}

// trustBefore is the change time before which a file of this filesystem is
// untouched since extraction; zero (trust nothing) if the clock has gone back
// past the extraction since, which could make later writes look older.
func (r *baseRecord) trustBefore(now time.Time) time.Time {
	if now.Before(r.Extracted) {
		return time.Time{}
	}
	m := r.Margin
	if m <= 0 {
		m = 2 * time.Second
	}
	return r.Extracted.Add(-m)
}

func (e *Engine) readBaseRecord(id string) (*baseRecord, error) {
	data, err := os.ReadFile(e.baseRecordPath(id))
	if err != nil {
		return nil, err
	}
	var r baseRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// writeBaseRecord records that the filesystem now in place came from base,
// extracted by then. Written only once that filesystem is in place: a crash
// before leaves an older record, whose earlier time makes the next save hash
// every file of the new filesystem instead of trusting it (slower, correct).
func (e *Engine) writeBaseRecord(id string, base save.BaseRef, extracted time.Time, margin time.Duration) error {
	data, err := json.Marshal(baseRecord{Base: base, Extracted: extracted.UTC(), Margin: margin})
	if err != nil {
		return err
	}
	tmp := e.baseRecordPath(id) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, e.baseRecordPath(id)); err != nil {
		return err
	}
	return syncDir(e.workspaceDir(id))
}

func (e *Engine) removeBaseRecord(id string) error {
	err := os.Remove(e.baseRecordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// uploadBase puts a base in object storage unless it is there already.
func (e *Engine) uploadBase(ctx context.Context, ref *save.BaseRef) error {
	keys, err := e.cfg.Objects.List(ctx, remoteBaseKey(ref.SHA256))
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k == remoteBaseKey(ref.SHA256) {
			return nil
		}
	}
	if err := e.verifyBase(ref); err != nil { // never upload a damaged base
		return err
	}
	f, err := os.Open(e.basePath(ref.SHA256))
	if err != nil {
		return err
	}
	defer f.Close()
	return e.cfg.Objects.Put(ctx, remoteBaseKey(ref.SHA256), f, ref.Size)
}

// fetchBase downloads a base and keeps it only if it hashes right.
func (e *Engine) fetchBase(ctx context.Context, ref *save.BaseRef) error {
	if err := e.needSpace("downloading a base", ref.Size); err != nil {
		return err
	}
	if err := os.MkdirAll(e.basesDir(), 0o700); err != nil {
		return err
	}
	tmp := e.basePath(ref.SHA256) + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	defer os.Remove(tmp)
	if err := e.getFile(ctx, remoteBaseKey(ref.SHA256), tmp); err != nil {
		return noSpace(err)
	}
	got, n, err := hashFileSHA(tmp)
	if err != nil {
		return err
	}
	if got != ref.SHA256 || n != ref.Size {
		return fmt.Errorf("%w: base %s from object storage does not match its checksum", save.ErrCorrupt, ref.SHA256)
	}
	if err := os.Rename(tmp, e.basePath(ref.SHA256)); err != nil {
		return err
	}
	e.log.Info("base downloaded and verified", "base", ref.SHA256, "store", e.cfg.Objects.String())
	return syncDir(e.basesDir())
}

// gcBases removes bases nothing refers to any more: no template, workspace
// or save on disk (deleted saves within retention included).
func (e *Engine) gcBases(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(e.basesDir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	used := map[string]bool{}
	ts, err := e.ListTemplates(ctx)
	if err != nil {
		return 0, err
	}
	for _, t := range ts {
		used[t.SHA256] = true
	}
	if dirs, err := os.ReadDir(filepath.Join(e.cfg.DataDir, "workspaces")); err == nil {
		for _, d := range dirs {
			if r, err := e.readBaseRecord(d.Name()); err == nil {
				used[r.Base.SHA256] = true
			}
		}
	}
	saves, err := os.ReadDir(e.savesDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	for _, d := range saves {
		// temporary save folders too: a save being written refers to its base
		// a save still being written has no manifest yet; the grace period
		// keeps any base it may use, and recovery removes it if abandoned
		data, err := os.ReadFile(filepath.Join(e.savesDir(), d.Name(), save.ManifestFile))
		if err != nil {
			continue
		}
		var m save.Manifest
		if json.Unmarshal(data, &m) == nil && m.Base != nil {
			used[m.Base.SHA256] = true
		}
	}
	n := 0
	cutoff := e.cfg.Now().Add(-baseGrace)
	for _, d := range entries {
		sha, ok := strings.CutSuffix(d.Name(), ".tar")
		if !ok || len(sha) != 64 || used[sha] {
			continue
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(e.basePath(sha)); err != nil {
			return n, err
		}
		_ = os.Remove(e.indexPath(sha))
		_ = os.Remove(e.indexPath(sha) + ".sha256")
		e.bases.mu.Lock()
		e.bases.init()
		delete(e.bases.indexes, sha)
		e.bases.mu.Unlock()
		n++
		e.log.Info("removed unused base", "base", sha)
	}
	return n, nil
}

func hashFileSHA(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = fsync(out)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
