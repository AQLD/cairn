package save

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestArchiveRoundTrip(t *testing.T) {
	src := t.TempDir()
	when := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.MkdirAll(filepath.Join(src, "a", "empty"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "a", "x.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.Chmod(filepath.Join(src, "a", "x.sh"), 0o751))
	must(t, os.Symlink("a/x.sh", filepath.Join(src, "link")))
	must(t, os.Chtimes(filepath.Join(src, "a", "x.sh"), when, when))

	var buf bytes.Buffer
	st, err := TarRoot(openRoot(t, src), &buf, nil)
	must(t, err)
	if st.Files != 1 || st.Dirs != 2 || st.Links != 1 || st.Bytes != 10 {
		t.Fatalf("stats %+v", st)
	}
	dst := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dst, "kept.txt"), []byte("kept"), 0o644))
	if _, err := ExtractTo(openRoot(t, dst), &buf, 1<<20); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, "a", "x.sh"))
	must(t, err)
	if fi.Mode().Perm() != 0o751 || !fi.ModTime().Equal(when) {
		t.Fatalf("mode %o time %v", fi.Mode().Perm(), fi.ModTime())
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "a/x.sh" {
		t.Fatalf("link %q %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "a", "empty")); err != nil {
		t.Fatal("empty folder lost")
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "kept.txt")); string(data) != "kept" {
		t.Fatal("extracting removed a file it did not carry")
	}
}

func TestTarRootSkip(t *testing.T) {
	src := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(src, "node_modules", "x"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "node_modules", "x", "i.js"), []byte("x"), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "keep.txt"), []byte("k"), 0o644))
	var buf bytes.Buffer
	st, err := TarRoot(openRoot(t, src), &buf, func(name string, _ os.DirEntry) bool { return name == "node_modules" })
	must(t, err)
	if st.Files != 1 || st.Dirs != 0 {
		t.Fatalf("skipped folder still archived: %+v", st)
	}
}

func tarOf(t *testing.T, hdrs ...*tar.Header) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg {
			h.Size = 1
		}
		must(t, tw.WriteHeader(h))
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte("x"))
		}
	}
	must(t, tw.Close())
	return bytes.NewReader(buf.Bytes())
}

func TestExtractToRefusesEscapes(t *testing.T) {
	outside := t.TempDir()
	cases := map[string][]*tar.Header{
		"climb":    {{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"absolute": {{Name: filepath.Join(outside, "evil"), Typeflag: tar.TypeReg, Mode: 0o644}},
		"hardlink": {{Name: "h", Linkname: "../../etc/passwd", Typeflag: tar.TypeLink}},
		"through symlink": {
			{Name: "s", Linkname: outside, Typeflag: tar.TypeSymlink},
			{Name: "s/evil", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		"over a folder": {
			{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "d/f", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "d", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		"over budget": {{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644}},
	}
	for name, hdrs := range cases {
		dst := t.TempDir()
		budget := int64(1 << 20)
		if name == "over budget" {
			budget = 0
		}
		_, err := ExtractTo(openRoot(t, dst), tarOf(t, hdrs...), budget)
		if !errors.Is(err, ErrBadArchive) {
			t.Errorf("%s: got %v, want ErrBadArchive", name, err)
		}
		if _, err := os.Stat(filepath.Join(outside, "evil")); err == nil {
			t.Fatalf("%s: wrote outside the folder", name)
		}
	}
}

func TestExtractToStripsSpecialBits(t *testing.T) {
	dst := t.TempDir()
	_, err := ExtractTo(openRoot(t, dst), tarOf(t, &tar.Header{Name: "suid", Typeflag: tar.TypeReg, Mode: 0o4755}), 1<<20)
	must(t, err)
	fi, err := os.Stat(filepath.Join(dst, "suid"))
	must(t, err)
	if fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

// TestUntarManyFilesInParallel: files are written on several goroutines
// (writers.go); the result must be exactly the archive, hard links to files
// written just before included (image tars carry them), and folders keep their
// final modes.
func TestUntarManyFilesInParallel(t *testing.T) {
	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	mtime := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := map[string]string{}
	dirs := map[string]bool{}
	for i := 0; i < 2000; i++ {
		dir := fmt.Sprintf("d%02d/e%d", i%37, i%5)
		for _, d := range []string{path.Dir(dir), dir} {
			if !dirs[d] {
				dirs[d] = true
				must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, ModTime: mtime}))
			}
		}
		name := fmt.Sprintf("%s/f%04d", dir, i)
		body := bytes.Repeat([]byte{byte(i)}, i%4096)
		must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: mtime}))
		_, err := tw.Write(body)
		must(t, err)
		want[name] = string(body)
		if i%97 == 0 { // a hard link to the file just written
			must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: name + ".link", Linkname: name}))
			want[name+".link"] = string(body)
		}
	}
	big := bytes.Repeat([]byte("x"), 3<<20) // over inlineMax: written by the reader
	must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "big.bin", Mode: 0o600, Size: int64(len(big)), ModTime: mtime}))
	_, err := tw.Write(big)
	must(t, err)
	want["big.bin"] = string(big)
	must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "ro/", Mode: 0o555, ModTime: mtime}))
	must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "ro/inside.txt", Mode: 0o444, Size: 2, ModTime: mtime}))
	_, err = tw.Write([]byte("hi"))
	must(t, err)
	want["ro/inside.txt"] = "hi"
	must(t, tw.Close())

	for round := 0; round < 3; round++ {
		got := filepath.Join(t.TempDir(), "out")
		must(t, UntarDir(bytes.NewReader(tarball.Bytes()), got, int64(tarball.Len())))
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(got, "ro"), 0o755) })
		for name, body := range want {
			b, err := os.ReadFile(filepath.Join(got, name))
			if err != nil || string(b) != body {
				t.Fatalf("round %d: %s: %d bytes, %v", round, name, len(b), err)
			}
			if fi, _ := os.Stat(filepath.Join(got, name)); !fi.ModTime().Equal(mtime) && !strings.HasSuffix(name, ".link") {
				t.Fatalf("round %d: %s mtime %v, want %v", round, name, fi.ModTime(), mtime)
			}
		}
		var a, b syscall.Stat_t
		must(t, syscall.Stat(filepath.Join(got, "d00/e0/f0000"), &a))
		must(t, syscall.Stat(filepath.Join(got, "d00/e0/f0000.link"), &b))
		if a.Ino != b.Ino {
			t.Fatalf("round %d: hard link not restored as a link", round)
		}
		if fi, _ := os.Stat(filepath.Join(got, "ro")); fi.Mode().Perm() != 0o555 {
			t.Fatalf("round %d: read-only folder mode %v", round, fi.Mode().Perm())
		}
	}
}
