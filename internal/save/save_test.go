package save

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildVerifyRoundTrip(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	must(t, os.MkdirAll(filepath.Join(src, "a/b"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "a/b/file.txt"), []byte("hello"), 0o640))
	must(t, os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.Symlink("a/b/file.txt", filepath.Join(src, "link")))

	b, err := NewBuilder(root, Manifest{SaveID: "sv_1", WorkspaceID: "ws_1", Kind: Quick, CreatedAt: time.Now()})
	must(t, err)
	var tarball bytes.Buffer
	must(t, TarDir(src, &tarball))
	must(t, b.WriteFile(RootfsFile, bytes.NewReader(tarball.Bytes())))
	m, err := b.Commit()
	must(t, err)
	if m.FormatVersion != FormatVersion || len(m.Files) != 1 {
		t.Fatalf("manifest: %+v", m)
	}

	if _, err := Verify(filepath.Join(root, "sv_1")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "dst")
	f, err := os.Open(filepath.Join(root, "sv_1", RootfsFile))
	must(t, err)
	defer f.Close()
	must(t, UntarDir(f, dst))
	got, err := os.ReadFile(filepath.Join(dst, "a/b/file.txt"))
	must(t, err)
	if string(got) != "hello" {
		t.Fatalf("content %q", got)
	}
	if info, _ := os.Stat(filepath.Join(dst, "run.sh")); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", info.Mode())
	}
	if target, _ := os.Readlink(filepath.Join(dst, "link")); target != "a/b/file.txt" {
		t.Fatalf("symlink %q", target)
	}
}

func TestAbortLeavesNothing(t *testing.T) {
	root := t.TempDir()
	b, err := NewBuilder(root, Manifest{SaveID: "sv_2"})
	must(t, err)
	must(t, b.WriteFile(RootfsFile, strings.NewReader("x")))
	b.Abort()
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("left %d entries", len(entries))
	}
}

func TestCleanupTempRemovesHalfWrittenSaves(t *testing.T) {
	root := t.TempDir()
	b, err := NewBuilder(root, Manifest{SaveID: "sv_3"})
	must(t, err)
	must(t, b.WriteFile(RootfsFile, strings.NewReader("partial")))
	// simulate a crash: no Commit, no Abort
	n, err := CleanupTemp(root)
	must(t, err)
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
}

func TestVerifyCatchesTampering(t *testing.T) {
	root := t.TempDir()
	b, err := NewBuilder(root, Manifest{SaveID: "sv_4"})
	must(t, err)
	must(t, b.WriteFile(RootfsFile, strings.NewReader("original content")))
	_, err = b.Commit()
	must(t, err)
	must(t, os.WriteFile(filepath.Join(root, "sv_4", RootfsFile), []byte("tampered content"), 0o600))
	if _, err := Verify(filepath.Join(root, "sv_4")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestBuilderRejectsUnsafeNames(t *testing.T) {
	for _, id := range []string{"", "../x", "a/b", ".hidden"} {
		if _, err := NewBuilder(t.TempDir(), Manifest{SaveID: id}); err == nil {
			t.Errorf("save id %q accepted", id)
		}
	}
	b, err := NewBuilder(t.TempDir(), Manifest{SaveID: "sv_5"})
	must(t, err)
	for _, name := range []string{"../x", "a/b", "", ".."} {
		if err := b.WriteFile(name, strings.NewReader("x")); err == nil {
			t.Errorf("file name %q accepted", name)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
