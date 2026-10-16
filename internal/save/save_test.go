package save

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
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
	must(t, UntarDir(f, dst, int64(tarball.Len())))
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

// A quick save reads a workspace that is still changing: files grow, shrink and
// disappear mid-archive. The archive must stay well-formed every time.
func TestTarDirWhileFilesChange(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 50; i++ {
		must(t, os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d", i)), bytes.Repeat([]byte("a"), 64<<10), 0o644))
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p := filepath.Join(src, fmt.Sprintf("f%d", i%50))
			switch i % 3 {
			case 0: // grow
				f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
				if err == nil {
					f.Write(bytes.Repeat([]byte("b"), 32<<10))
					f.Close()
				}
			case 1: // shrink
				os.Truncate(p, 100)
			case 2: // vanish and come back
				os.Remove(p)
				os.WriteFile(p, []byte("back"), 0o644)
			}
		}
	}()
	for round := 0; round < 30; round++ {
		var buf bytes.Buffer
		if err := TarDir(src, &buf); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if err := UntarDir(bytes.NewReader(buf.Bytes()), filepath.Join(t.TempDir(), "out"), int64(buf.Len())); err != nil {
			t.Fatalf("round %d: archive does not extract: %v", round, err)
		}
	}
	close(stop)
	<-done
}

// An archive whose entries claim more bytes than the archive holds is refused
// before anything big is written.
func TestUntarDirRefusesInflatedArchives(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	must(t, tw.WriteHeader(&tar.Header{Name: "huge", Mode: 0o644, Size: 1 << 40, Typeflag: tar.TypeReg}))
	// no body: the header alone claims a terabyte
	_ = tw.Flush()
	err := UntarDir(bytes.NewReader(buf.Bytes()), t.TempDir(), int64(buf.Len()))
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

// `tar -C dir -cf - .` names the root "./" and every entry "./..."; such
// archives (how most root filesystems are made) extract normally.
func TestUntarDirAcceptsDotPrefixedArchives(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	must(t, tw.WriteHeader(&tar.Header{Name: "./", Mode: 0o755, Typeflag: tar.TypeDir}))
	must(t, tw.WriteHeader(&tar.Header{Name: "./etc/", Mode: 0o755, Typeflag: tar.TypeDir}))
	must(t, tw.WriteHeader(&tar.Header{Name: "./etc/hostname", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg}))
	_, _ = tw.Write([]byte("ws"))
	must(t, tw.Close())
	out := t.TempDir()
	must(t, UntarDir(bytes.NewReader(buf.Bytes()), out, int64(buf.Len())))
	got, err := os.ReadFile(filepath.Join(out, "etc/hostname"))
	must(t, err)
	if string(got) != "ws" {
		t.Fatalf("got %q", got)
	}
	// a regular file posing as the root is still refused
	buf.Reset()
	tw = tar.NewWriter(&buf)
	must(t, tw.WriteHeader(&tar.Header{Name: ".", Mode: 0o644, Typeflag: tar.TypeReg}))
	must(t, tw.Close())
	if err := UntarDir(bytes.NewReader(buf.Bytes()), t.TempDir(), int64(buf.Len())); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}
