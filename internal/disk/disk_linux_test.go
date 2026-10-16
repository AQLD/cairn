//go:build linux

package disk

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestImageEnforcesSizeAndKeepsFiles(t *testing.T) {
	if err := Supported(); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	img, mnt := filepath.Join(dir, "disk.img"), filepath.Join(dir, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Create(img, 64); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(img); err != nil {
		t.Fatal(err)
	} else if used := info.Sys().(*syscall.Stat_t).Blocks * 512; used > 16<<20 {
		t.Fatalf("empty 64 MB image uses %d MB of host disk; want it sparse", used>>20)
	}
	if err := Mount(img, mnt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Unmount(mnt) })
	if ok, err := Mounted(mnt); err != nil || !ok {
		t.Fatalf("Mounted = %v, %v after Mount", ok, err)
	}

	if err := os.WriteFile(filepath.Join(mnt, "keep"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(mnt, "fill"))
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	var written int64
	for {
		n, err := f.Write(chunk)
		written += int64(n)
		if err != nil {
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("want ENOSPC, got %v", err)
			}
			break
		}
		if written > 128<<20 {
			t.Fatalf("wrote %d MB into a 64 MB image", written>>20)
		}
	}
	f.Close()

	if err := Unmount(mnt); err != nil {
		t.Fatal(err)
	}
	if ok, _ := Mounted(mnt); ok {
		t.Fatal("still mounted after Unmount")
	}
	if err := Unmount(mnt); err != nil {
		t.Fatalf("Unmount of an unmounted folder: %v", err)
	}
	if err := Mount(img, mnt); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(mnt, "keep")); err != nil || string(b) != "kept" {
		t.Fatalf("after remount: %q, %v", b, err)
	}
}

func TestCreateRefusesTinySizes(t *testing.T) {
	if err := Create(filepath.Join(t.TempDir(), "x.img"), MinSizeMB-1); err == nil {
		t.Fatal("want an error below MinSizeMB")
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{`/a\040b`: "/a b", `/plain`: "/plain", `/end\04`: `/end\04`} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}
