package save

import (
	"sort"
)

// ChangeKind says how a path differs between two saves.
type ChangeKind string

const (
	Added    ChangeKind = "added"
	Modified ChangeKind = "modified"
	Removed  ChangeKind = "removed"
)

// Change is one path that differs between two saves' filesystems.
type Change struct {
	// Path is absolute, as the workspace sees it ("/root/app.py").
	Path   string     `json:"path"`
	Change ChangeKind `json:"change"`
	// Type is "file", "dir" or "symlink": what the path is in the later save,
	// or was, for a removed path.
	Type string `json:"type"`
	// Size is a file's size in the later save (the earlier one if removed).
	Size int64 `json:"size,omitempty"`
}

// Diff lists the paths that differ from one save's filesystem (from) to
// another's (to), sorted by path. A path is modified when its type, content,
// link target, permissions or (with compareOwner) owner differ; ownership
// means something only where saves restore it, which needs root, as
// WriteDelta and UntarDir do. Modification times alone do not
// count: a folder's time moves whenever anything in it is added or removed,
// which the entries inside already show, and a file rewritten with the same
// bytes has not changed for anyone reading it.
func Diff(from, to *Index, compareOwner bool) []Change {
	var out []Change
	for p, t := range to.Entries {
		f, ok := from.Entries[p]
		switch {
		case !ok:
			out = append(out, change(p, Added, t))
		case differs(f, t, compareOwner):
			out = append(out, change(p, Modified, t))
		}
	}
	for p, f := range from.Entries {
		if _, ok := to.Entries[p]; !ok {
			out = append(out, change(p, Removed, f))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func differs(a, b IndexEntry, compareOwner bool) bool {
	if a.Type != b.Type || a.Mode != b.Mode {
		return true
	}
	if compareOwner && (a.UID != b.UID || a.GID != b.GID) {
		return true
	}
	switch a.Type {
	case 'f':
		return a.SHA256 != b.SHA256 || a.Size != b.Size
	case 'l':
		return a.Link != b.Link
	}
	return false
}

func change(p string, kind ChangeKind, e IndexEntry) Change {
	c := Change{Path: "/" + p, Change: kind}
	switch e.Type {
	case 'f':
		c.Type, c.Size = "file", e.Size
	case 'd':
		c.Type = "dir"
	case 'l':
		c.Type = "symlink"
	}
	return c
}
