package save

// Saves of workspaces made from a template store their filesystem as a delta
// against that template (the base): what was added, changed or removed since
// the workspace was extracted from it. A save of a 1 GB toolchain image after
// `pip install` is then the installed packages, not the whole image again.
//
// The base is identified by the SHA-256 of its tar, so a save restores even
// after the template that named it is replaced or deleted: the engine keeps
// the base while anything refers to it.
//
// Deciding what changed without reading every file: a regular file whose
// size, mode, owner and modification time (to the second) match the base, and
// whose inode change time (ctime) is older than the moment the workspace's
// filesystem was extracted, has not been touched since: nothing can write a
// file, or change its metadata, without moving its ctime to "now", and ctime
// cannot be set back. Every other file is hashed and compared. A stale or
// missing extraction time only makes more files get hashed.

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DeltaFile holds a save's filesystem as a delta against its base.
const DeltaFile = "rootfs.delta.tar"

// whiteoutKey marks a delta entry that removes a path present in the base.
const whiteoutKey = "CAIRN.whiteout"

// BaseRef names the base a delta applies to.
type BaseRef struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (b *BaseRef) valid() error {
	if b == nil {
		return nil
	}
	if len(b.SHA256) != 64 || strings.Trim(b.SHA256, "0123456789abcdef") != "" || b.Size < 0 {
		return fmt.Errorf("invalid base %q", b.SHA256)
	}
	return nil
}

// IndexEntry is one path of a base, as extracting it produces it.
type IndexEntry struct {
	Type   byte   `json:"t"` // 'd' folder, 'f' file, 'l' symlink
	Mode   uint32 `json:"m"` // permission and setuid/setgid/sticky bits
	UID    int    `json:"u"`
	GID    int    `json:"g"`
	Size   int64  `json:"s,omitempty"`
	Mtime  int64  `json:"mt"`          // Unix seconds
	SHA256 string `json:"h,omitempty"` // files
	Link   string `json:"l,omitempty"` // symlinks
}

// Index lists every path of a base.
type Index struct {
	Entries map[string]IndexEntry `json:"entries"`
}

const modeBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// BuildIndex reads a base tar (as UntarDir would extract it) and records
// every path. Hard links get their target's entry: they are the same file.
func BuildIndex(r io.Reader) (*Index, error) {
	idx := &Index{Entries: map[string]IndexEntry{}}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return idx, nil
		}
		if err != nil {
			return nil, err
		}
		name := path.Clean(strings.TrimSuffix(hdr.Name, "/"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			continue
		}
		e := IndexEntry{Mode: uint32(hdr.FileInfo().Mode() & modeBits), UID: hdr.Uid, GID: hdr.Gid, Mtime: hdr.ModTime.Unix()}
		switch hdr.Typeflag {
		case tar.TypeDir:
			e.Type = 'd'
		case tar.TypeReg:
			h := sha256.New()
			n, err := io.Copy(h, tr)
			if err != nil {
				return nil, err
			}
			e.Type, e.Size, e.SHA256 = 'f', n, hex.EncodeToString(h.Sum(nil))
		case tar.TypeSymlink:
			e.Type, e.Link = 'l', hdr.Linkname
		case tar.TypeLink:
			target, ok := idx.Entries[path.Clean(strings.TrimPrefix(hdr.Linkname, "./"))]
			if !ok {
				continue
			}
			e = target
		default:
			continue // not extracted (devices, fifos)
		}
		idx.Entries[name] = e
	}
}

// WriteIndex and ReadIndex store an index as JSON.
func WriteIndex(w io.Writer, idx *Index) error { return json.NewEncoder(w).Encode(idx) }

func ReadIndex(r io.Reader) (*Index, error) {
	var idx Index
	if err := json.NewDecoder(r).Decode(&idx); err != nil {
		return nil, err
	}
	if idx.Entries == nil {
		return nil, errors.New("index has no entries")
	}
	return &idx, nil
}

// DeltaStats describes a delta just written.
type DeltaStats struct {
	Changed, Removed, Hashed int
	Bytes                    int64
}

// WriteDelta writes, as a tar stream, how dir differs from the base idx
// describes. untouchedBefore is when dir's filesystem was extracted from that
// base (files whose ctime is older are not read; zero reads every candidate).
// compareOwner says whether ownership can differ meaningfully (restoring it
// needs root, as UntarDir does).
//
// Like TarDir, it may read a running workspace: a file being written at that
// moment can be torn. Full saves freeze or stop the workspace first.
func WriteDelta(dir string, idx *Index, untouchedBefore time.Time, compareOwner bool, w io.Writer) (DeltaStats, error) {
	var st DeltaStats
	tw := tar.NewWriter(w)
	seen := make(map[string]byte, len(idx.Entries)) // path → its type now
	emitted := map[string]bool{}                    // folders already written
	// writeDir writes a folder's header (its metadata), once.
	writeDir := func(name string) error {
		if emitted[name] || name == "." {
			return nil
		}
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name + "/"
		clearOwner(hdr)
		emitted[name] = true
		return tw.WriteHeader(hdr)
	}
	// ancestors: applying an entry changes its parent folders' times, so
	// their metadata travels with it and is set again last
	ancestors := func(name string) error {
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			if err := writeDir(strings.Join(parts[:i], "/")); err != nil {
				return err
			}
		}
		return nil
	}

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while we walked
			}
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var typ byte
		switch {
		case info.IsDir():
			typ = 'd'
		case info.Mode().IsRegular():
			typ = 'f'
		case info.Mode()&fs.ModeSymlink != 0:
			typ = 'l'
		default:
			return nil // not saved (devices, fifos, sockets), as TarDir
		}
		seen[name] = typ
		base, inBase := idx.Entries[name]
		same := inBase && base.Type == typ && base.Mode == uint32(info.Mode()&modeBits) &&
			(!compareOwner || ownerMatches(info, base))
		link := ""
		switch {
		case !same:
		case typ == 'd':
			same = info.ModTime().Unix() == base.Mtime
		case typ == 'l':
			if link, err = os.Readlink(p); err != nil {
				return nil // replaced meanwhile
			}
			same = link == base.Link
		case typ == 'f':
			same = info.Size() == base.Size && info.ModTime().Unix() == base.Mtime
			if same && !changeTime(info).Before(untouchedBefore) {
				st.Hashed++
				sum, err := hashPath(p)
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				same = sum == base.SHA256
			}
		}
		if same {
			return nil
		}
		if err := ancestors(name); err != nil {
			return err
		}
		st.Changed++
		switch typ {
		case 'd':
			return writeDir(name)
		case 'f':
			n, err := tarFileCounted(tw, p, name)
			st.Bytes += n
			return err
		default:
			if link == "" {
				if link, err = os.Readlink(p); err != nil {
					return nil
				}
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = name
			clearOwner(hdr)
			return tw.WriteHeader(hdr)
		}
	})
	if err != nil {
		return st, err
	}

	// paths of the base that are gone: one whiteout for the topmost of each
	var gone []string
	for name := range idx.Entries {
		if seen[name] == 0 && !replacedAbove(name, seen) {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	removed := map[string]bool{}
	for _, name := range gone {
		if parent := path.Dir(name); parent != "." && underRemoved(parent, removed) {
			continue
		}
		removed[name] = true
		if err := ancestors(name); err != nil {
			return st, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Format: tar.FormatPAX,
			PAXRecords: map[string]string{whiteoutKey: "1"}}); err != nil {
			return st, err
		}
		st.Removed++
	}
	return st, tw.Close()
}

// replacedAbove reports whether a parent of name is no longer a folder: the
// entry that replaced it already removed everything under it, and a whiteout
// must not reach through what replaced it (a symlink, say).
func replacedAbove(name string, seen map[string]byte) bool {
	for p := path.Dir(name); p != "."; p = path.Dir(p) {
		if t := seen[p]; t != 0 && t != 'd' {
			return true
		}
	}
	return false
}

func underRemoved(p string, removed map[string]bool) bool {
	for p != "." && p != "/" {
		if removed[p] {
			return true
		}
		p = path.Dir(p)
	}
	return false
}

// ApplyDelta applies a delta written by WriteDelta to dir, which holds its
// base. maxBytes bounds the file content it may write (the delta's size).
func ApplyDelta(r io.Reader, dir string, maxBytes int64) error {
	return extract(r, dir, maxBytes, true)
}

// tarFileCounted is tarFile, reporting the bytes of content written.
func tarFileCounted(tw *tar.Writer, p, name string) (int64, error) {
	info, err := os.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), tarFile(tw, p, name)
}

func hashPath(p string) (string, error) {
	sum, _, err := hashFile(p)
	return sum, err
}

// ChangeTimeMargin is how far before a filesystem's extraction time its
// files' change times must be for WriteDelta to trust them: the clock's
// coarseness on filesystems that keep nanoseconds (writes are stamped from a
// coarse clock, a tick or so behind), a whole second plus on those that keep
// only seconds. It looks at what dir's own entries record.
func ChangeTimeMargin(dir string) time.Duration {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 2 * time.Second
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && changeTime(info).Nanosecond() != 0 {
			return 100 * time.Millisecond
		}
	}
	return 2 * time.Second
}
