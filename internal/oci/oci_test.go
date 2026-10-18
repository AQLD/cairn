package oci_test

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AQLD/cairn/internal/oci"
	"github.com/AQLD/cairn/internal/oci/ocitest"
)

func TestParseReference(t *testing.T) {
	cases := []struct{ in, want string }{
		{"python", "registry-1.docker.io/library/python:latest"},
		{"python:3.12-slim", "registry-1.docker.io/library/python:3.12-slim"},
		{"docker.io/team/app:v1", "registry-1.docker.io/team/app:v1"},
		{"ghcr.io/aqld/agent:2026", "ghcr.io/aqld/agent:2026"},
		{"localhost:5000/tools", "localhost:5000/tools:latest"},
		{"registry.internal:8443/a/b/c:tag", "registry.internal:8443/a/b/c:tag"},
		{"ghcr.io/x/y@sha256:" + strings.Repeat("ab", 32), "ghcr.io/x/y@sha256:" + strings.Repeat("ab", 32)},
	}
	for _, c := range cases {
		r, err := oci.ParseReference(c.in)
		if err != nil || r.String() != c.want {
			t.Errorf("%q: got %q, %v; want %q", c.in, r.String(), err, c.want)
		}
	}
	for _, bad := range []string{"", "Upper/Case", "x@sha256:short", "a b"} {
		if _, err := oci.ParseReference(bad); !errors.Is(err, oci.ErrInvalid) {
			t.Errorf("%q: want ErrInvalid, got %v", bad, err)
		}
	}
}

// pull resolves and flattens host/repo:tag into a temp folder.
func pull(t *testing.T, reg *ocitest.Registry, repo string, auth oci.Auth) (string, error) {
	t.Helper()
	ref, err := oci.ParseReference(reg.Host() + "/" + repo)
	if err != nil {
		t.Fatal(err)
	}
	p := &oci.Puller{Insecure: true}
	img, err := p.Resolve(context.Background(), ref, auth)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(t.TempDir(), "rootfs")
	return dir, p.Flatten(context.Background(), img, auth, dir, 1<<20)
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

func gone(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should not exist (err %v)", name, err)
	}
}

func TestLayersApplyInOrderWithWhiteouts(t *testing.T) {
	reg := ocitest.New()
	defer reg.Close()
	base := ocitest.Layer(
		ocitest.Entry{Name: "etc/", Type: tar.TypeDir},
		ocitest.Entry{Name: "etc/os-release", Body: "base\n"},
		ocitest.Entry{Name: "etc/keep", Body: "kept\n"},
		ocitest.Entry{Name: "etc/remove-me", Body: "x"},
		ocitest.Entry{Name: "cache/", Type: tar.TypeDir},
		ocitest.Entry{Name: "cache/old-1", Body: "1"},
		ocitest.Entry{Name: "cache/sub/old-2", Body: "2"},
		ocitest.Entry{Name: "bin/tool", Body: "#!/bin/sh\n", Mode: 0o4755},
		ocitest.Entry{Name: "bin/tool-link", Type: tar.TypeLink, Linkname: "bin/tool"},
		ocitest.Entry{Name: "bin/sh", Type: tar.TypeSymlink, Linkname: "/bin/busybox"},
	)
	top := ocitest.Layer(
		ocitest.Entry{Name: "etc/os-release", Body: "top\n"},                 // overwrites
		ocitest.Entry{Name: "etc/remove-me", Whiteout: true},                 // deletes
		ocitest.Entry{Name: "cache/new", Body: "new"},                        // written before the marker...
		ocitest.Entry{Name: "cache", Opaque: true},                           // ...which hides only lower layers
		ocitest.Entry{Name: "etc/remove-me-too", Whiteout: true},             // nothing to delete: fine
		ocitest.Entry{Name: "readonly/", Type: tar.TypeDir, Mode: 0o555},     // read-only folder...
		ocitest.Entry{Name: "readonly/file", Body: "inside a read-only dir"}, // ...still filled
	)
	reg.Push("team/app", "v1", true, base, top)
	dir, err := pull(t, reg, "team/app:v1", oci.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, "etc/os-release"); got != "top\n" {
		t.Errorf("os-release = %q, want the upper layer's", got)
	}
	read(t, dir, "etc/keep")
	gone(t, dir, "etc/remove-me")
	gone(t, dir, "cache/old-1")
	gone(t, dir, "cache/sub")
	if got := read(t, dir, "cache/new"); got != "new" {
		t.Errorf("cache/new = %q: the opaque marker removed this layer's own file", got)
	}
	fi, err := os.Stat(filepath.Join(dir, "bin/tool"))
	if err != nil || fi.Mode()&os.ModeSetuid == 0 {
		t.Errorf("bin/tool lost its setuid bit: %v %v", fi.Mode(), err)
	}
	a, _ := os.Stat(filepath.Join(dir, "bin/tool"))
	b, _ := os.Stat(filepath.Join(dir, "bin/tool-link"))
	if !os.SameFile(a, b) {
		t.Error("hard link not kept")
	}
	if l, _ := os.Readlink(filepath.Join(dir, "bin/sh")); l != "/bin/busybox" {
		t.Errorf("symlink = %q", l)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "readonly")); fi.Mode().Perm() != 0o555 {
		t.Errorf("readonly/ mode = %v, want 0555 applied after its contents", fi.Mode().Perm())
	}
	_ = os.Chmod(filepath.Join(dir, "readonly"), 0o755) // let the test clean up
}

func TestAuthentication(t *testing.T) {
	layer := ocitest.Layer(ocitest.Entry{Name: "hello", Body: "hi"})
	for _, mode := range []ocitest.Auth{ocitest.Basic, ocitest.Token} {
		reg := ocitest.New()
		reg.Auth, reg.Username, reg.Password = mode, "robot", "s3cret"
		reg.Push("private/img", "latest", true, layer)

		if _, err := pull(t, reg, "private/img", oci.Auth{}); !errors.Is(err, oci.ErrDenied) {
			t.Errorf("auth %d, anonymous: want ErrDenied, got %v", mode, err)
		}
		if _, err := pull(t, reg, "private/img", oci.Auth{Username: "robot", Password: "wrong"}); !errors.Is(err, oci.ErrDenied) {
			t.Errorf("auth %d, wrong password: want ErrDenied, got %v", mode, err)
		}
		dir, err := pull(t, reg, "private/img", oci.Auth{Username: "robot", Password: "s3cret"})
		if err != nil {
			t.Fatalf("auth %d: %v", mode, err)
		}
		if read(t, dir, "hello") != "hi" {
			t.Errorf("auth %d: wrong content", mode)
		}
		reg.Close()
	}
}

func TestMultiPlatformPicksThisArchitecture(t *testing.T) {
	reg := ocitest.New()
	defer reg.Close()
	other := "amd64"
	if runtime.GOARCH == "amd64" {
		other = "arm64"
	}
	reg.PushIndex("multi", "v1", map[string][]byte{
		"linux/" + runtime.GOARCH: ocitest.Layer(ocitest.Entry{Name: "arch", Body: runtime.GOARCH}),
		"linux/" + other:          ocitest.Layer(ocitest.Entry{Name: "arch", Body: other}),
	})
	dir, err := pull(t, reg, "multi:v1", oci.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, "arch"); got != runtime.GOARCH {
		t.Errorf("got the %s image, want %s", got, runtime.GOARCH)
	}

	reg.PushIndex("foreign", "v1", map[string][]byte{"linux/s390x": ocitest.Layer(ocitest.Entry{Name: "x", Body: "x"})})
	if _, err := pull(t, reg, "foreign:v1", oci.Auth{}); !errors.Is(err, oci.ErrInvalid) || !strings.Contains(err.Error(), "linux/s390x") {
		t.Errorf("want ErrInvalid naming the available platforms, got %v", err)
	}
}

func TestRefusals(t *testing.T) {
	reg := ocitest.New()
	defer reg.Close()
	ok := ocitest.Layer(ocitest.Entry{Name: "f", Body: "content"})

	if _, err := pull(t, reg, "nope:v1", oci.Auth{}); !errors.Is(err, oci.ErrNotFound) {
		t.Errorf("missing image: want ErrNotFound, got %v", err)
	}

	for _, gz := range []bool{false, true} {
		reg.Push("bad", "v1", gz, ok)
		reg.Corrupt(reg.LayerDigests("bad", "v1")[0])
		if _, err := pull(t, reg, "bad:v1", oci.Auth{}); !errors.Is(err, oci.ErrCorrupt) {
			t.Errorf("corrupt blob (gzip %v): want ErrCorrupt, got %v", gz, err)
		}
	}

	pinned := ocitest.Layer(ocitest.Entry{Name: "pinned", Body: "by digest"}) // not the corrupted blob above
	d := reg.Push("pinned", "v1", false, pinned)
	if _, err := pull(t, reg, "pinned@"+d, oci.Auth{}); err != nil {
		t.Errorf("pull by digest: %v", err)
	}
	wrong := "sha256:" + strings.Repeat("0", 64)
	reg.Push("pinned", wrong, false, pinned) // a manifest stored under a digest it does not have
	if _, err := pull(t, reg, "pinned@"+wrong, oci.Auth{}); !errors.Is(err, oci.ErrCorrupt) {
		t.Errorf("manifest under the wrong digest: want ErrCorrupt, got %v", err)
	}

	reg.PushRaw("zstd", "v1", "application/vnd.oci.image.layer.v1.tar+zstd", ok)
	if _, err := pull(t, reg, "zstd:v1", oci.Auth{}); !errors.Is(err, oci.ErrInvalid) || !strings.Contains(err.Error(), "zstd") {
		t.Errorf("zstd layer: want ErrInvalid naming zstd, got %v", err)
	}

	reg.Push("escape", "v1", false, ocitest.Layer(ocitest.Entry{Name: "../../etc/passwd", Body: "x"}))
	if _, err := pull(t, reg, "escape:v1", oci.Auth{}); !errors.Is(err, oci.ErrInvalid) {
		t.Errorf("path escape: want ErrInvalid, got %v", err)
	}
	reg.Push("escape-wh", "v1", false, ocitest.Layer(ocitest.Entry{Name: "../outside", Whiteout: true}))
	if _, err := pull(t, reg, "escape-wh:v1", oci.Auth{}); !errors.Is(err, oci.ErrInvalid) {
		t.Errorf("whiteout escape: want ErrInvalid, got %v", err)
	}

	reg.Push("huge", "v1", true, ocitest.Layer(ocitest.Entry{Name: "big", Body: strings.Repeat("x", 2<<20)}))
	if _, err := pull(t, reg, "huge:v1", oci.Auth{}); !errors.Is(err, oci.ErrInvalid) || !strings.Contains(err.Error(), "larger") {
		t.Errorf("over the size budget: want ErrInvalid, got %v", err)
	}
}
