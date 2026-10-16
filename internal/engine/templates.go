package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/AQLD/cairn/internal/save"
)

// A template is a root filesystem that new workspaces start from (a Python or
// Node toolchain, say). It is stored once as a tar file; every workspace gets
// its own extracted copy, so a workspace can never change its template.
//
//	templates/<name>/rootfs.tar
//	templates/<name>/template.json

var templateName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type Template struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

func (e *Engine) templatesDir() string           { return filepath.Join(e.cfg.DataDir, "templates") }
func (e *Engine) templateDir(name string) string { return filepath.Join(e.templatesDir(), name) }

// ImportTemplate stores a root filesystem tar (for example `docker export` of
// an image) as a template, replacing any template of the same name. Workspaces
// already created from the old one keep their copy.
func (e *Engine) ImportTemplate(ctx context.Context, name string, r io.Reader) (*Template, error) {
	if !templateName.MatchString(name) {
		return nil, fmt.Errorf("%w: template names are lower-case letters, digits, '.', '_' and '-'", ErrInvalid)
	}
	release, err := e.heavy(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := os.MkdirAll(e.templatesDir(), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(e.templatesDir(), ".tmp-"+name+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp) // no-op once renamed

	f, err := os.Create(filepath.Join(tmp, "rootfs.tar"))
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = fsync(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}

	// Prove it extracts (paths, size) before anything is created from it.
	probe := filepath.Join(tmp, "probe")
	tf, err := os.Open(filepath.Join(tmp, "rootfs.tar"))
	if err != nil {
		return nil, err
	}
	err = save.UntarDir(tf, probe, n)
	tf.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: template is not a usable root filesystem tar: %v", ErrInvalid, err)
	}
	if err := os.RemoveAll(probe); err != nil {
		return nil, err
	}

	t := Template{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), CreatedAt: e.cfg.Now().UTC()}
	meta, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "template.json"), meta, 0o600); err != nil {
		return nil, err
	}
	if err := syncFile(filepath.Join(tmp, "template.json")); err != nil {
		return nil, err
	}
	if err := syncDir(tmp); err != nil {
		return nil, err
	}
	// Swap in: old version aside, new one into place, old one removed.
	final, old := e.templateDir(name), e.templateDir(name)+".old"
	_ = os.RemoveAll(old)
	if err := os.Rename(final, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Rename(old, final)
		return nil, err
	}
	if err := syncDir(e.templatesDir()); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(old)
	return &t, nil
}

func (e *Engine) GetTemplate(_ context.Context, name string) (*Template, error) {
	if !templateName.MatchString(name) {
		return nil, fmt.Errorf("%w: template %q", ErrNotFound, name)
	}
	data, err := os.ReadFile(filepath.Join(e.templateDir(name), "template.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: template %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	var t Template
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (e *Engine) ListTemplates(ctx context.Context) ([]Template, error) {
	entries, err := os.ReadDir(e.templatesDir())
	if errors.Is(err, os.ErrNotExist) {
		return []Template{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Template{}
	for _, d := range entries {
		if !d.IsDir() || !templateName.MatchString(d.Name()) {
			continue // temp and old folders
		}
		if t, err := e.GetTemplate(ctx, d.Name()); err == nil {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (e *Engine) DeleteTemplate(_ context.Context, name string) error {
	if !templateName.MatchString(name) {
		return fmt.Errorf("%w: template %q", ErrNotFound, name)
	}
	if _, err := os.Stat(e.templateDir(name)); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: template %q", ErrNotFound, name)
	}
	return os.RemoveAll(e.templateDir(name))
}

// createRootfs gives a new workspace its filesystem: empty, or a copy of a
// template. It is built next to the final place and renamed in, so a crash
// never leaves a half-extracted workspace.
func (e *Engine) createRootfs(id string, spec Spec) error {
	root := e.volume(id, "rootfs", spec)
	if root.exists() {
		return root.attach() // already there (recovery after the rename)
	}
	if spec.Template == "" && !spec.DiskImage {
		return os.MkdirAll(root.path, 0o755)
	}
	tmp := e.volume(id, "rootfs.new", spec)
	if err := tmp.remove(); err != nil {
		return err
	}
	var f *os.File
	var size int64
	if spec.Template != "" {
		var err error
		f, err = os.Open(filepath.Join(e.templateDir(spec.Template), "rootfs.tar"))
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: template %q", ErrNotFound, spec.Template)
		}
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		size = info.Size()
	}
	err := tmp.prepare()
	if err == nil && f != nil {
		if err = save.UntarDir(f, tmp.path, size); errors.Is(diskFull(err, spec), ErrInsufficientStorage) {
			err = fmt.Errorf("%w: template %q does not fit in a %d MB disk", ErrInvalid, spec.Template, spec.Limits.DiskMB)
		}
	}
	if err == nil {
		err = tmp.release()
	}
	if err != nil {
		_ = tmp.remove()
		return err
	}
	if err := tmp.renameTo(root); err != nil {
		return err
	}
	return root.attach()
}
