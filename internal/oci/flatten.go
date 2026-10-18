package oci

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// layerCompression is how a layer's blob is compressed: "gzip" or "".
func layerCompression(mediaType string) (string, error) {
	switch {
	case strings.HasSuffix(mediaType, ".tar.gzip"), strings.HasSuffix(mediaType, "tar+gzip"):
		return "gzip", nil
	case strings.HasSuffix(mediaType, ".tar"), strings.HasSuffix(mediaType, "layer.v1.tar"):
		return "", nil
	case strings.HasSuffix(mediaType, "+zstd"):
		return "", fmt.Errorf("%w: zstd-compressed layers are not supported yet (rebuild with gzip)", ErrInvalid)
	default:
		return "", fmt.Errorf("%w: unsupported layer type %q", ErrInvalid, mediaType)
	}
}

// Flatten downloads the image's layers, bottom first, and applies them to dir
// (empty or missing): the image's root filesystem, whiteouts applied. Content
// beyond maxBytes of file data is refused, so an image cannot fill the disk.
func (p *Puller) Flatten(ctx context.Context, img *Image, auth Auth, dir string, maxBytes int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	a := &applier{root: root, budget: maxBytes, dirs: map[string]dirMeta{}, asRoot: os.Geteuid() == 0}
	for i, l := range img.Layers {
		if err := a.layer(ctx, p, img, l, auth); err != nil {
			return fmt.Errorf("layer %d (%s): %w", i+1, l.Digest, err)
		}
	}
	return a.finish()
}

type dirMeta struct {
	mode  fs.FileMode
	mtime time.Time
}

type applier struct {
	root   *os.Root
	budget int64
	asRoot bool
	// Folder modes and times are set once every layer is in: later layers
	// write into folders an earlier one may have made read-only.
	dirs map[string]dirMeta
}

func (a *applier) layer(ctx context.Context, p *Puller, img *Image, d Descriptor, auth Auth) error {
	comp, err := layerCompression(d.MediaType)
	if err != nil {
		return err
	}
	blob, err := p.Layer(ctx, img, d, auth)
	if err != nil {
		return err
	}
	defer blob.Close()
	var r io.Reader = blob
	if comp == "gzip" {
		gz, err := gzip.NewReader(blob)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		defer gz.Close()
		r = gz
	}
	if err := a.apply(tar.NewReader(r)); err != nil {
		return err
	}
	// Read to the very end (tar padding, gzip trailer, then the raw blob) so the
	// digest is checked before the layer counts as applied.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, blob)
	return err
}

// apply writes one layer's entries over what the layers below left.
func (a *applier) apply(tr *tar.Reader) error {
	created := map[string]bool{} // paths this layer wrote: an opaque marker keeps them
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		name := path.Clean(strings.TrimPrefix(strings.TrimSuffix(hdr.Name, "/"), "./"))
		if name == "." || name == "/" {
			continue
		}
		if !safe(name) {
			return fmt.Errorf("%w: unsafe path %q", ErrInvalid, hdr.Name)
		}
		dir, base := path.Dir(name), path.Base(name)

		// Whiteouts: an opaque marker hides everything the lower layers put in
		// its folder; .wh.NAME deletes NAME.
		if base == ".wh..wh..opq" {
			if err := a.opaque(dir, created); err != nil {
				return err
			}
			continue
		}
		if victim, ok := strings.CutPrefix(base, ".wh."); ok {
			target := path.Join(dir, victim)
			if !safe(target) {
				return fmt.Errorf("%w: unsafe whiteout %q", ErrInvalid, hdr.Name)
			}
			if err := a.root.RemoveAll(target); err != nil {
				return err
			}
			delete(a.dirs, target)
			continue
		}

		if dir != "." {
			if err := a.root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		// A new entry replaces whatever was there, except a folder over a folder.
		if fi, err := a.root.Lstat(name); err == nil {
			if !(fi.IsDir() && hdr.Typeflag == tar.TypeDir) {
				if err := a.root.RemoveAll(name); err != nil {
					return err
				}
			}
		}
		mode := hdr.FileInfo().Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := a.root.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if err := a.chown(name, hdr); err != nil {
				return err
			}
			a.dirs[name] = dirMeta{mode, hdr.ModTime}
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > a.budget {
				return fmt.Errorf("%w: the image is larger than the %d bytes allowed", ErrInvalid, a.budget)
			}
			a.budget -= hdr.Size
			f, err := a.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			if err := a.meta(name, hdr, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// os.Root never follows a link out of the folder: any target is safe to store.
			if err := a.root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
			if a.asRoot {
				if err := a.root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
					return err
				}
			}
		case tar.TypeLink:
			target := path.Clean(strings.TrimPrefix(hdr.Linkname, "./"))
			if !safe(target) {
				return fmt.Errorf("%w: unsafe link target %q", ErrInvalid, hdr.Linkname)
			}
			if err := a.root.Link(target, name); err != nil {
				return err
			}
		default:
			// devices and fifos: the sandbox provides /dev
			continue
		}
		created[name] = true
	}
}

// opaque empties dir of what lower layers put there, keeping this layer's own.
func (a *applier) opaque(dir string, created map[string]bool) error {
	if dir != "." {
		if err := a.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := a.root.Open(dir)
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := path.Join(dir, e.Name())
		if created[child] {
			continue
		}
		if err := a.root.RemoveAll(child); err != nil {
			return err
		}
		for d := range a.dirs {
			if d == child || strings.HasPrefix(d, child+"/") {
				delete(a.dirs, d)
			}
		}
	}
	return nil
}

func (a *applier) chown(name string, hdr *tar.Header) error {
	if a.asRoot {
		return a.root.Lchown(name, hdr.Uid, hdr.Gid)
	}
	return nil
}

// meta applies ownership, the exact mode (special bits too) and the time.
func (a *applier) meta(name string, hdr *tar.Header, mode fs.FileMode) error {
	if err := a.chown(name, hdr); err != nil {
		return err
	}
	if err := a.root.Chmod(name, mode); err != nil { // after chown, which clears setuid
		return err
	}
	_ = a.root.Chtimes(name, hdr.ModTime, hdr.ModTime)
	return nil
}

func (a *applier) finish() error {
	names := make([]string, 0, len(a.dirs))
	for n := range a.dirs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) }) // deepest first
	for _, n := range names {
		m := a.dirs[n]
		if err := a.root.Chmod(n, m.mode); err != nil {
			return err
		}
		_ = a.root.Chtimes(n, m.mtime, m.mtime)
	}
	return nil
}

func safe(p string) bool {
	return p != "." && p != ".." && !path.IsAbs(p) && !strings.HasPrefix(p, "../")
}
