package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/AQLD/cairn/internal/oci"
	"github.com/AQLD/cairn/internal/save"
)

// A template is a root filesystem that new workspaces start from (a Python or
// Node toolchain, say). It is stored once as a tar file. Where overlays work,
// workspaces share one read-only extracted copy of it (its base's root) and
// store only their own changes; elsewhere each gets its own extracted copy.
// Either way a workspace can never change its template.
//
//	templates/<name>/rootfs.tar
//	templates/<name>/template.json

var templateName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type Template struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	// Image and ImageDigest name the container image a pulled template was
	// built from (empty for an imported tar).
	Image       string `json:"image,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
}

func (e *Engine) templatesDir() string           { return filepath.Join(e.cfg.DataDir, "templates") }
func (e *Engine) templateDir(name string) string { return filepath.Join(e.templatesDir(), name) }

// ImportTemplate stores a root filesystem tar (for example `docker export` of
// an image) as a template, replacing any template of the same name. Workspaces
// already created from the old one keep their copy.
func (e *Engine) ImportTemplate(ctx context.Context, name string, r io.Reader) (*Template, error) {
	return e.importTemplate(ctx, name, r, Template{})
}

// importTemplate stores the tar from r as template name; src carries where it
// came from (Image, ImageDigest).
func (e *Engine) importTemplate(ctx context.Context, name string, r io.Reader, src Template) (*Template, error) {
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

	t := Template{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), CreatedAt: e.cfg.Now().UTC(),
		Image: src.Image, ImageDigest: src.ImageDigest}
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
	// keep it as a base too, indexed now rather than at the first save
	if err := e.ensureBase(t.SHA256, filepath.Join(final, "rootfs.tar")); err != nil {
		e.log.Warn("template kept without a base: its workspaces' saves hold their whole filesystem", "template", name, "err", err)
	} else if _, err := e.baseIndex(t.SHA256); err != nil {
		e.log.Warn("could not index base", "template", name, "err", err)
	}
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
	if spec.Template != "" && e.overlayErr == nil {
		done, err := e.createLayeredRootfs(id, spec, root, tmp)
		if done || err != nil {
			return err
		}
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
	extracted := time.Now()
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
	if err := root.attach(); err != nil {
		return err
	}
	if f != nil {
		e.recordTemplateBase(id, spec.Template, f, size, extracted)
	}
	return nil
}

// createLayeredRootfs gives a new workspace a filesystem layered on its
// template's shared root. It returns done=false, with nothing built, when
// the template cannot be shared this time (its base cannot be made), so the
// caller copies it as before.
func (e *Engine) createLayeredRootfs(id string, spec Spec, root, tmp volume) (bool, error) {
	t, err := e.GetTemplate(context.Background(), spec.Template)
	if err != nil {
		return false, err
	}
	tar := filepath.Join(e.templateDir(spec.Template), "rootfs.tar")
	if err := e.ensureBase(t.SHA256, tar); err != nil {
		e.log.Warn("template cannot be shared; copying it", "template", spec.Template, "err", err)
		return false, nil
	}
	if _, err := e.ensureRoot(t.SHA256); err != nil {
		e.log.Warn("template cannot be shared; copying it", "template", spec.Template, "err", err)
		return false, nil
	}
	tmp.lower = t.SHA256
	err = tmp.prepare()
	extracted := time.Now() // every file from the base is older than this
	if err == nil {
		err = tmp.release()
	}
	if err != nil {
		_ = tmp.remove()
		return true, err
	}
	if err := tmp.renameTo(root); err != nil {
		return true, err
	}
	if err := root.attach(); err != nil {
		return true, err
	}
	margin := save.ChangeTimeMargin(root.path)
	if err := e.writeBaseRecord(id, save.BaseRef{SHA256: t.SHA256, Size: t.Size}, extracted, margin); err != nil {
		e.log.Warn("could not record workspace base", "workspace", id, "err", err)
	}
	return true, nil
}

// recordTemplateBase records that a new workspace's filesystem is the
// template's tar f, so its saves can be deltas. Without the record its saves
// hold the whole filesystem, so failures here cost space, never correctness.
func (e *Engine) recordTemplateBase(id, template string, f *os.File, size int64, extracted time.Time) {
	t, err := e.GetTemplate(context.Background(), template)
	if err != nil {
		return
	}
	// the tar extracted is the template's only if the template was not
	// replaced meanwhile: same file now as the one opened
	opened, err1 := f.Stat()
	current, err2 := os.Stat(filepath.Join(e.templateDir(template), "rootfs.tar"))
	if err1 != nil || err2 != nil || !os.SameFile(opened, current) || t.Size != size {
		return
	}
	if err := e.ensureBase(t.SHA256, filepath.Join(e.templateDir(template), "rootfs.tar")); err != nil {
		e.log.Warn("no base for workspace: its saves hold their whole filesystem", "workspace", id, "err", err)
		return
	}
	margin := save.ChangeTimeMargin(e.volume(id, "rootfs", Spec{}).path)
	if err := e.writeBaseRecord(id, save.BaseRef{SHA256: t.SHA256, Size: t.Size}, extracted, margin); err != nil {
		e.log.Warn("could not record workspace base", "workspace", id, "err", err)
	}
}

// PullTemplate is a request to build a template from a container image.
type PullTemplate struct {
	Image string // e.g. registry.internal/team/python:3.12
	// Registry login for this pull only: never stored or logged.
	Username, Password string
	// Insecure talks plain HTTP to the registry (a registry without TLS on a
	// private network).
	Insecure bool
}

// maxImageBytes caps the file content an image may expand to.
const maxImageBytes = 32 << 30

// PullTemplate builds template name from a container image in a registry: the
// image for this server's architecture, its layers applied in order with their
// deletions, every blob checked against its digest. The result is stored
// exactly like an imported tar, so workspaces cannot tell the difference.
func (e *Engine) PullTemplate(ctx context.Context, name string, req PullTemplate) (*Template, error) {
	if !templateName.MatchString(name) {
		return nil, fmt.Errorf("%w: template names are lower-case letters, digits, '.', '_' and '-'", ErrInvalid)
	}
	ref, err := oci.ParseReference(req.Image)
	if err != nil {
		return nil, registryErr(err)
	}
	auth := oci.Auth{Username: req.Username, Password: req.Password}
	p := &oci.Puller{Insecure: req.Insecure}
	img, err := p.Resolve(ctx, ref, auth)
	if err != nil {
		return nil, registryErr(err)
	}
	if err := os.MkdirAll(e.templatesDir(), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(e.templatesDir(), ".tmp-pull-"+name+"-")
	if err != nil {
		return nil, err
	}
	defer removeTree(tmp)
	rootfs := filepath.Join(tmp, "rootfs")
	if err := p.Flatten(ctx, img, auth, rootfs, maxImageBytes); err != nil {
		return nil, registryErr(err)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(save.TarDir(rootfs, pw)) }()
	t, err := e.importTemplate(ctx, name, pr, Template{Image: ref.String(), ImageDigest: img.Digest})
	pr.CloseWithError(err)
	return t, err
}

// registryErr maps registry failures onto the engine's error codes. A refused
// login is invalid_argument, not unauthorized: that code is about Cairn's own
// API token.
func registryErr(err error) error {
	switch {
	case errors.Is(err, oci.ErrNotFound):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, oci.ErrInvalid), errors.Is(err, oci.ErrDenied), errors.Is(err, oci.ErrCorrupt):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return err
}

// removeTree deletes a folder an image filled, read-only folders included.
func removeTree(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
