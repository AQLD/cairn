package save

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// Folder archives move a project folder between a person's machine and a
// workspace (push and pull). Unlike a save's filesystem image they are merged
// into a folder that may already hold files, and they carry no ownership: the
// files belong to whoever extracts them.

// ErrBadArchive means a pushed archive is not a usable tar stream, or names a
// path outside the folder it is extracted into.
var ErrBadArchive = errors.New("bad archive")

// ArchiveStats counts what an archive held.
type ArchiveStats struct {
	Files int   `json:"files"`
	Dirs  int   `json:"dirs"`
	Links int   `json:"links"`
	Bytes int64 `json:"bytes"`
}

// TarRoot writes everything under root as a tar stream. Every access goes
// through root, so a symlink inside the folder (even one named in the path the
// caller opened root at) can never pull in files from outside it. Symlinks are
// stored as links. skip, if set, leaves out an entry (a whole folder when it is
// one); name is slash-separated and relative to root.
func TarRoot(root *os.Root, w io.Writer, skip func(name string, d fs.DirEntry) bool) (ArchiveStats, error) {
	var st ArchiveStats
	tw := tar.NewWriter(w)
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while we walked
			}
			return err
		}
		if name == "." {
			return nil
		}
		if skip != nil && skip(name, d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular():
			n, err := tarRootFile(tw, root, name)
			if err == nil && n >= 0 {
				st.Files++
				st.Bytes += n
			}
			return err
		case info.IsDir():
			st.Dirs++
			return writeHeader(tw, info, name+"/", "")
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := root.Readlink(name)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			st.Links++
			return writeHeader(tw, info, name, link)
		default:
			return nil // devices, fifos and sockets are not project files
		}
	})
	if err != nil {
		return st, err
	}
	return st, tw.Close()
}

func writeHeader(tw *tar.Writer, info fs.FileInfo, name, link string) error {
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	hdr.Name = name
	noOwner(hdr)
	return tw.WriteHeader(hdr)
}

// noOwner drops ownership: an archive's files belong to whoever extracts them.
func noOwner(hdr *tar.Header) {
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
}

// tarRootFile adds one regular file, sized by its open handle (see tarFile).
// It returns -1 when the file went away or changed type since the walk saw it.
func tarRootFile(tw *tar.Writer, root *os.Root, name string) (int64, error) {
	f, err := root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return -1, nil
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	hdr.Name = name
	noOwner(hdr)
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, err
	}
	n, err := io.CopyN(tw, f, info.Size())
	if errors.Is(err, io.EOF) {
		_, err = io.CopyN(tw, zeroReader{}, info.Size()-n)
	}
	return info.Size(), err
}

// ExtractTo merges a tar stream into root: folders are created or kept, and a
// file, link or symlink replaces whatever non-folder had its name. Modes keep
// their permission bits only (no setuid, setgid or sticky), times are kept and
// ownership is not: files belong to the caller. Entries that would land
// outside root are rejected with ErrBadArchive, and so is an archive whose
// file content exceeds maxBytes.
func ExtractTo(root *os.Root, r io.Reader, maxBytes int64) (ArchiveStats, error) {
	var st ArchiveStats
	budget := maxBytes
	type dirMeta struct {
		name  string
		mode  fs.FileMode
		mtime time.Time
	}
	var dirs []dirMeta
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// folder modes and times last: filling a folder changes its time, and
			// a read-only folder could not be filled
			for i := len(dirs) - 1; i >= 0; i-- {
				if err := root.Chmod(dirs[i].name, dirs[i].mode); err != nil {
					return st, err
				}
				_ = root.Chtimes(dirs[i].name, dirs[i].mtime, dirs[i].mtime)
			}
			return st, nil
		}
		if err != nil {
			return st, fmt.Errorf("%w: %v", ErrBadArchive, err)
		}
		name, err := archiveName(hdr.Name)
		if err != nil {
			return st, err
		}
		if name == "." {
			continue // the folder itself, as `tar -C dir -cf - .` writes it
		}
		mode := hdr.FileInfo().Mode() & fs.ModePerm
		if dir := path.Dir(name); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return st, fmt.Errorf("%w: %s: %v", ErrBadArchive, name, err)
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if fi, err := root.Lstat(name); err == nil && !fi.IsDir() {
				if err := root.Remove(name); err != nil {
					return st, err
				}
			}
			if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return st, fmt.Errorf("%w: %s: %v", ErrBadArchive, name, err)
			}
			st.Dirs++
			dirs = append(dirs, dirMeta{name, mode, hdr.ModTime})
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > budget {
				return st, fmt.Errorf("%w: %q is %d bytes, over the limit", ErrBadArchive, hdr.Name, hdr.Size)
			}
			budget -= hdr.Size
			if err := replaceable(root, name); err != nil {
				return st, err
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return st, fmt.Errorf("%w: %s: %v", ErrBadArchive, name, err)
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return st, err
			}
			if err := root.Chmod(name, mode); err != nil {
				return st, err
			}
			_ = root.Chtimes(name, hdr.ModTime, hdr.ModTime)
			st.Files++
			st.Bytes += hdr.Size
		case tar.TypeSymlink:
			if err := replaceable(root, name); err != nil {
				return st, err
			}
			// os.Root never follows a link out of root, so any target is safe to store
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return st, fmt.Errorf("%w: %s: %v", ErrBadArchive, name, err)
			}
			st.Links++
		case tar.TypeLink:
			target, err := archiveName(hdr.Linkname)
			if err != nil || target == "." {
				return st, fmt.Errorf("%w: unsafe link target %q", ErrBadArchive, hdr.Linkname)
			}
			if err := replaceable(root, name); err != nil {
				return st, err
			}
			if err := root.Link(target, name); err != nil {
				return st, fmt.Errorf("%w: %s: %v", ErrBadArchive, name, err)
			}
			st.Files++
		default:
			continue // devices, fifos and the like are not project files
		}
	}
}

// archiveName cleans an entry's name, rejecting any that leaves the folder.
func archiveName(name string) (string, error) {
	n := path.Clean(strings.TrimSuffix(strings.ReplaceAll(name, `\`, "/"), "/"))
	if path.IsAbs(n) || n == ".." || strings.HasPrefix(n, "../") {
		return "", fmt.Errorf("%w: unsafe path %q", ErrBadArchive, name)
	}
	return n, nil
}

// replaceable clears the way for a new entry: a file or link of the same name
// goes, a folder stays and the entry is refused (it would drop the folder's
// contents).
func replaceable(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("%w: %s is a folder", ErrBadArchive, name)
	}
	return root.Remove(name)
}
