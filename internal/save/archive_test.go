package save

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
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
