package save

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzUntarDir: no archive, however malformed, may write outside the target
// folder or crash the engine.
func FuzzUntarDir(f *testing.F) {
	for _, entries := range [][]tar.Header{
		{{Name: "a.txt", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg}},
		{{Name: "../escape", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}},
		{{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}, {Name: "link/x", Size: 1, Mode: 0o644, Typeflag: tar.TypeReg}},
		{{Name: "d/", Mode: 0o755, Typeflag: tar.TypeDir}, {Name: "d/../../x", Size: 1, Mode: 0o644, Typeflag: tar.TypeReg}},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range entries {
			h := h
			_ = tw.WriteHeader(&h)
			if h.Size > 0 {
				_, _ = tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
			}
		}
		_ = tw.Close()
		f.Add(buf.Bytes())
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		_ = UntarDir(bytes.NewReader(data), target, int64(len(data))) // errors are fine; escapes are not
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "target" {
				t.Fatalf("archive wrote %q outside the target folder", e.Name())
			}
		}
		if n, _ := dirSizeForTest(target); n > int64(len(data)) {
			t.Fatalf("archive of %d bytes expanded to %d bytes", len(data), n)
		}
	})
}

// FuzzVerify: a damaged or hostile manifest is refused, never trusted and never
// a crash; file names in it can never point outside the save folder.
func FuzzVerify(f *testing.F) {
	good, _ := json.Marshal(Manifest{FormatVersion: 1, SaveID: "sv_1", Files: []File{{Name: RootfsFile, Size: 3, SHA256: strings.Repeat("0", 64)}}})
	f.Add(good)
	f.Add([]byte(`{"format_version":1,"files":[{"name":"../../etc/passwd","size":1,"sha256":"x"}]}`))
	f.Add([]byte(`{"format_version":999}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, manifest []byte) {
		dir := t.TempDir()
		saveDir := filepath.Join(dir, "sv")
		if err := os.MkdirAll(saveDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(saveDir, ManifestFile), manifest, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(saveDir, RootfsFile), []byte("abc"), 0o600); err != nil {
			t.Fatal(err)
		}
		m, err := Verify(saveDir)
		if err == nil {
			// only a manifest whose every file checks out may pass
			for _, file := range m.Files {
				if file.Name != filepath.Base(file.Name) {
					t.Fatalf("verified a manifest naming %q", file.Name)
				}
			}
		}
	})
}

func dirSizeForTest(root string) (int64, error) {
	var n int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n, err
}
