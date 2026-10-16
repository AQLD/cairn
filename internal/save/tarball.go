package save

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// TarDir writes the contents of dir to w as a tar stream. Symlinks are stored
// as links, never followed, so a link inside a workspace cannot pull host files
// into a save.
//
// The workspace may be running while this reads it (a quick save). Files that
// disappear during the walk are skipped; each file is copied at the size its
// open handle reports, ignoring growth after that and zero-padding if it shrinks
// mid-copy. A file being written at that moment can therefore be torn, exactly as
// after a sudden power cut. Full saves pause the workspace and are exact.
func TarDir(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while we walked
			}
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		if d.Type().IsRegular() {
			return tarFile(tw, p, name)
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); errors.Is(err, fs.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		clearOwner(hdr)
		return tw.WriteHeader(hdr)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// tarFile adds one regular file, sized by its open handle so concurrent writes
// cannot make the archive inconsistent.
func tarFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil // replaced by something else since the walk saw it
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	hdr.Name = name
	clearOwner(hdr)
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.CopyN(tw, f, info.Size())
	if errors.Is(err, io.EOF) {
		// shrank mid-copy: keep the archive well-formed
		_, err = io.CopyN(tw, zeroReader{}, info.Size()-n)
	}
	return err
}

// clearOwner keeps numeric ownership (a workspace's files belong to its own
// users, which a save must keep) and drops the host's names for them, which
// mean nothing inside a workspace.
func clearOwner(hdr *tar.Header) {
	hdr.Uname, hdr.Gname = "", ""
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// UntarDir extracts a tar stream written by TarDir into dir, which must be
// empty or missing. Entries that would land outside dir are rejected, and so
// is any archive that would expand beyond maxBytes of file content: TarDir
// never writes sparse files, so real content can never exceed the archive's own
// size, and a crafted archive must not be able to fill the disk.
func UntarDir(r io.Reader, dir string, maxBytes int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	budget := maxBytes
	// Folder modes and times are set last: creating their children would change
	// the times, and a read-only folder could not receive them.
	type dirMeta struct {
		name  string
		mode  fs.FileMode
		mtime time.Time
	}
	var dirs []dirMeta
	asRoot := os.Geteuid() == 0
	// meta applies what a create call cannot: special bits (setuid, setgid,
	// sticky), the exact mode regardless of umask, and ownership when we can.
	meta := func(name string, hdr *tar.Header, mode fs.FileMode) error {
		if asRoot {
			if err := root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
				return err
			}
		}
		if hdr.Typeflag != tar.TypeSymlink {
			// after chown, which clears setuid and setgid
			return root.Chmod(name, mode)
		}
		return nil
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			for i := len(dirs) - 1; i >= 0; i-- {
				if err := root.Chmod(dirs[i].name, dirs[i].mode); err != nil {
					return err
				}
				_ = root.Chtimes(dirs[i].name, dirs[i].mtime, dirs[i].mtime)
			}
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimSuffix(hdr.Name, "/"))
		if name == "." && hdr.Typeflag == tar.TypeDir {
			continue // the root itself, as `tar -C dir -cf - .` writes it
		}
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("%w: unsafe path %q in filesystem image", ErrCorrupt, hdr.Name)
		}
		mode := hdr.FileInfo().Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		if dirName := path.Dir(name); dirName != "." {
			if err := root.MkdirAll(dirName, 0o755); err != nil {
				return err
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if asRoot {
				if err := root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
					return err
				}
			}
			dirs = append(dirs, dirMeta{name, mode, hdr.ModTime})
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > budget {
				return fmt.Errorf("%w: %q claims %d bytes, more than the archive holds", ErrCorrupt, hdr.Name, hdr.Size)
			}
			budget -= hdr.Size
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
			if err := meta(name, hdr, mode); err != nil {
				return err
			}
			// keep modification times: a restored filesystem must look like the one
			// a checkpoint was taken against
			_ = root.Chtimes(name, hdr.ModTime, hdr.ModTime)
		case tar.TypeSymlink:
			// os.Root never follows a link out of dir, so storing any target is safe here.
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
			if err := meta(name, hdr, mode); err != nil {
				return err
			}
		case tar.TypeLink:
			// Hard links (common in images: perl, git, coreutils) point at an entry
			// already extracted; os.Root keeps both ends inside dir.
			target := path.Clean(strings.TrimPrefix(hdr.Linkname, "./"))
			if target == "." || path.IsAbs(target) || target == ".." || strings.HasPrefix(target, "../") {
				return fmt.Errorf("%w: unsafe link target %q in filesystem image", ErrCorrupt, hdr.Linkname)
			}
			if err := root.Link(target, name); err != nil {
				return err
			}
		case tar.TypeGNUSparse:
			return fmt.Errorf("%w: sparse entry %q (Cairn never writes sparse files)", ErrCorrupt, hdr.Name)
		default:
			// Devices and fifos are not part of a workspace filesystem (/dev is
			// provided by the sandbox).
			continue
		}
	}
}
