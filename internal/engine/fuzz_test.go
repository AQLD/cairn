package engine

import (
	"path"
	"strings"
	"testing"
)

// FuzzWorkspacePath: whatever a caller sends as a file path, the cleaned path is
// relative and never climbs out of the workspace (os.Root then enforces
// symlinks on top of this).
func FuzzWorkspacePath(f *testing.F) {
	for _, s := range []string{"a/b.txt", "../../etc/passwd", "/etc/passwd", `..\..\x`, "a/../../b", "", ".", "./a/./b/", "a\x00b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		rel, err := workspacePath(name)
		if err != nil {
			return
		}
		if rel == "" || path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") || path.Clean(rel) != rel {
			t.Fatalf("workspacePath(%q) = %q", name, rel)
		}
	})
}
