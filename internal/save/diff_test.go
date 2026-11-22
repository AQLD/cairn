package save

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A save stored as a delta is listed without extracting it: the base's index
// with the delta applied must describe exactly the folder the delta was
// written from, and the diff against the base must name every change.
func TestIndexWithDeltaAndDiff(t *testing.T) {
	tmpl := t.TempDir()
	for name, content := range map[string]string{
		"keep.txt":           "unchanged",
		"same-size.txt":      "aaaa",
		"edit.txt":           "before",
		"gone.txt":           "bye",
		"chmod.sh":           "#!/bin/sh",
		"tree/x/y/z.txt":     "deep",
		"becomes-file/f.txt": "f",
		"becomes-link/f.txt": "f",
		"file-to-dir":        "f",
		"dir/touched.txt":    "same bytes",
	} {
		write(t, filepath.Join(tmpl, name), content, 0o644)
	}
	must(t, os.Symlink("keep.txt", filepath.Join(tmpl, "link")))
	var baseTar bytes.Buffer
	must(t, TarDir(tmpl, &baseTar))
	base, err := BuildIndex(bytes.NewReader(baseTar.Bytes()))
	must(t, err)

	ws := t.TempDir()
	must(t, UntarDir(bytes.NewReader(baseTar.Bytes()), ws, int64(baseTar.Len())))
	extracted := time.Now()
	time.Sleep(20 * time.Millisecond)
	must(t, os.WriteFile(filepath.Join(ws, "same-size.txt"), []byte("bbbb"), 0o644))
	write(t, filepath.Join(ws, "edit.txt"), "after, longer", 0o644)
	must(t, os.Remove(filepath.Join(ws, "gone.txt")))
	must(t, os.Chmod(filepath.Join(ws, "chmod.sh"), 0o755))
	write(t, filepath.Join(ws, "added/deep/n.txt"), "n", 0o644)
	must(t, os.RemoveAll(filepath.Join(ws, "tree")))
	must(t, os.Remove(filepath.Join(ws, "link")))
	must(t, os.Symlink("edit.txt", filepath.Join(ws, "link")))
	must(t, os.RemoveAll(filepath.Join(ws, "becomes-file")))
	write(t, filepath.Join(ws, "becomes-file"), "now a file", 0o644)
	must(t, os.RemoveAll(filepath.Join(ws, "becomes-link")))
	must(t, os.Symlink("/etc", filepath.Join(ws, "becomes-link")))
	must(t, os.Remove(filepath.Join(ws, "file-to-dir")))
	write(t, filepath.Join(ws, "file-to-dir/inside.txt"), "i", 0o644)
	// rewritten with the same bytes and a new time: not a change
	write(t, filepath.Join(ws, "dir/touched.txt"), "same bytes", 0o644)
	later := time.Now().Add(time.Hour)
	must(t, os.Chtimes(filepath.Join(ws, "dir/touched.txt"), later, later))

	var delta bytes.Buffer
	_, err = WriteDelta(ws, base, extracted, false, &delta)
	must(t, err)
	got, err := IndexWithDelta(base, bytes.NewReader(delta.Bytes()))
	must(t, err)

	var wsTar bytes.Buffer
	must(t, TarDir(ws, &wsTar))
	want, err := BuildIndex(bytes.NewReader(wsTar.Bytes()))
	must(t, err)
	if d := Diff(want, got, true); len(d) != 0 {
		t.Fatalf("index with delta differs from the folder: %v", d)
	}

	var lines []string
	for _, c := range Diff(base, got, false) {
		lines = append(lines, fmt.Sprintf("%s %s %s", c.Change, c.Type, c.Path))
	}
	wantLines := []string{
		"added dir /added",
		"added dir /added/deep",
		"added file /added/deep/n.txt",
		"modified file /becomes-file",
		"removed file /becomes-file/f.txt",
		"modified symlink /becomes-link",
		"removed file /becomes-link/f.txt",
		"modified file /chmod.sh",
		"modified file /edit.txt",
		"modified dir /file-to-dir",
		"added file /file-to-dir/inside.txt",
		"removed file /gone.txt",
		"modified symlink /link",
		"modified file /same-size.txt",
		"removed dir /tree",
		"removed dir /tree/x",
		"removed dir /tree/x/y",
		"removed file /tree/x/y/z.txt",
	}
	if g, w := strings.Join(lines, "\n"), strings.Join(wantLines, "\n"); g != w {
		t.Fatalf("diff\n--- want\n%s\n--- got\n%s", w, g)
	}
	if d := Diff(got, got, true); len(d) != 0 {
		t.Fatalf("a save differs from itself: %v", d)
	}
}
