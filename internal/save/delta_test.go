package save

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// treeListing describes everything a save must keep about a tree: type, mode,
// size, content, link target and (for files and folders) the modification
// time to the nearest second (tar headers round it, in every save format).
func treeListing(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %v", filepath.ToSlash(rel), info.Mode()&(modeBits|fs.ModeDir|fs.ModeSymlink))
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			line += " -> " + l
		case info.Mode().IsRegular():
			data, _ := os.ReadFile(p)
			h := sha256.Sum256(data)
			line += fmt.Sprintf(" %d %s %d", len(data), hex.EncodeToString(h[:8]), info.ModTime().Round(time.Second).Unix())
		case info.IsDir():
			line += fmt.Sprintf(" %d", info.ModTime().Round(time.Second).Unix())
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(content), mode))
}

func TestDeltaRoundTrip(t *testing.T) {
	// the template
	tmpl := t.TempDir()
	old := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, content := range map[string]string{
		"keep.txt":           "unchanged",
		"same-size.txt":      "aaaa",
		"edit.txt":           "before",
		"gone.txt":           "bye",
		"chmod.sh":           "#!/bin/sh",
		"lib/a.py":           "a",
		"lib/sub/b.py":       "b",
		"tree/x/y/z.txt":     "deep",
		"becomes-file/f.txt": "f",
		"becomes-link/f.txt": "f",
		"file-to-dir":        "f",
		"big/untouched.bin":  strings.Repeat("x", 100_000),
	} {
		write(t, filepath.Join(tmpl, name), content, 0o644)
	}
	must(t, os.Symlink("keep.txt", filepath.Join(tmpl, "link")))
	_ = filepath.WalkDir(tmpl, func(p string, d fs.DirEntry, err error) error {
		if d.Type()&fs.ModeSymlink == 0 {
			_ = os.Chtimes(p, old, old)
		}
		return nil
	})
	var baseTar bytes.Buffer
	must(t, TarDir(tmpl, &baseTar))
	idx, err := BuildIndex(bytes.NewReader(baseTar.Bytes()))
	must(t, err)

	// a workspace made from it
	ws := t.TempDir()
	must(t, UntarDir(bytes.NewReader(baseTar.Bytes()), ws, int64(baseTar.Len())))
	extracted := time.Now()
	time.Sleep(20 * time.Millisecond)

	// what an agent does
	samePath := filepath.Join(ws, "same-size.txt")
	must(t, os.WriteFile(samePath, []byte("bbbb"), 0o644))
	must(t, os.Chtimes(samePath, old, old)) // same size, same mtime: only ctime tells
	write(t, filepath.Join(ws, "edit.txt"), "after, longer", 0o644)
	must(t, os.Remove(filepath.Join(ws, "gone.txt")))
	must(t, os.Chmod(filepath.Join(ws, "chmod.sh"), 0o755))
	write(t, filepath.Join(ws, "lib/sub/new.py"), "new", 0o644)
	write(t, filepath.Join(ws, "added/deep/n.txt"), "n", 0o600)
	must(t, os.RemoveAll(filepath.Join(ws, "tree")))
	must(t, os.Remove(filepath.Join(ws, "link")))
	must(t, os.Symlink("edit.txt", filepath.Join(ws, "link")))
	must(t, os.RemoveAll(filepath.Join(ws, "becomes-file")))
	write(t, filepath.Join(ws, "becomes-file"), "now a file", 0o644)
	must(t, os.RemoveAll(filepath.Join(ws, "becomes-link")))
	must(t, os.Symlink("/etc", filepath.Join(ws, "becomes-link"))) // points outside: must never be followed
	must(t, os.Remove(filepath.Join(ws, "file-to-dir")))
	write(t, filepath.Join(ws, "file-to-dir/inside.txt"), "i", 0o644)
	want := treeListing(t, ws)

	for _, tc := range []struct {
		name      string
		untouched time.Time
	}{{"ctime", extracted}, {"no extraction time", time.Time{}}} {
		t.Run(tc.name, func(t *testing.T) {
			var delta bytes.Buffer
			st, err := WriteDelta(ws, idx, tc.untouched, false, &delta)
			must(t, err)
			if st.Bytes >= 100_000 {
				t.Fatalf("delta carries %d bytes: the untouched big file went in", st.Bytes)
			}
			if tc.name == "ctime" && st.Hashed > 2 {
				t.Fatalf("hashed %d files; only those changed since extraction should be", st.Hashed)
			}
			got := t.TempDir()
			must(t, UntarDir(bytes.NewReader(baseTar.Bytes()), got, int64(baseTar.Len())))
			must(t, ApplyDelta(bytes.NewReader(delta.Bytes()), got, int64(delta.Len())))
			if g := treeListing(t, got); g != want {
				t.Fatalf("restored tree differs\n--- want\n%s\n--- got\n%s", want, g)
			}
		})
	}
}

func TestApplyDeltaRefusesEscapes(t *testing.T) {
	base := t.TempDir()
	must(t, os.Symlink("/etc", filepath.Join(base, "out")))
	var delta bytes.Buffer
	tw := newTestTar(&delta)
	tw.file("out/passwd", "pwned")
	tw.close()
	if err := ApplyDelta(bytes.NewReader(delta.Bytes()), base, int64(delta.Len())); err == nil {
		t.Fatal("a delta wrote through a symlink out of the workspace")
	}
	if _, err := os.Stat(filepath.Join(base, "out", "passwd")); err == nil {
		data, _ := os.ReadFile("/etc/passwd")
		if string(data) == "pwned" {
			t.Fatal("wrote outside the workspace")
		}
	}
}

type testTar struct {
	tw *tar.Writer
}

func newTestTar(w *bytes.Buffer) *testTar { return &testTar{tw: tar.NewWriter(w)} }

func (t *testTar) file(name, content string) {
	_ = t.tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
	_, _ = t.tw.Write([]byte(content))
}

func (t *testTar) close() { _ = t.tw.Close() }
