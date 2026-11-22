package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AQLD/cairn/internal/save"
)

// SaveDiff is what changed in the files from one save to another.
type SaveDiff struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Added    int    `json:"added"`
	Modified int    `json:"modified"`
	Removed  int    `json:"removed"`
	// Changes lists the changed paths, sorted, at most the limit asked for;
	// Truncated says some were left out (the counts above include them).
	Changes   []save.Change `json:"changes"`
	Truncated bool          `json:"truncated,omitempty"`
}

// DefaultDiffLimit is how many changes a diff lists unless asked otherwise.
const DefaultDiffLimit = 1000

// DiffSaves lists the files added, modified and removed from save from to
// save to. With from empty it compares to with the save it came from (its
// parent). Both saves are verified first, as for a restore: a damaged save
// would list changes that are not there. Memory and processes are not
// compared, only files.
func (e *Engine) DiffSaves(ctx context.Context, to, from string, limit int) (*SaveDiff, error) {
	if limit <= 0 {
		limit = DefaultDiffLimit
	}
	if from == "" {
		sv, err := e.store.GetSave(ctx, to)
		if err != nil {
			return nil, err
		}
		if sv.Parent == "" {
			return nil, fmt.Errorf("%w: save %s has no earlier save to compare with; name one", ErrInvalid, to)
		}
		from = sv.Parent
	}
	release, err := e.heavy(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	fromIdx, err := e.saveIndex(ctx, from)
	if err != nil {
		return nil, err
	}
	toIdx, err := e.saveIndex(ctx, to)
	if err != nil {
		return nil, err
	}
	out := &SaveDiff{From: from, To: to, Changes: []save.Change{}}
	// ownership is restored (and so compared) only as root, as saves do
	for _, c := range save.Diff(fromIdx, toIdx, os.Geteuid() == 0) {
		switch c.Change {
		case save.Added:
			out.Added++
		case save.Modified:
			out.Modified++
		case save.Removed:
			out.Removed++
		}
		if len(out.Changes) < limit {
			out.Changes = append(out.Changes, c)
		} else {
			out.Truncated = true
		}
	}
	return out, nil
}

// saveIndex lists every path of a save's filesystem, read from the save
// without extracting it.
func (e *Engine) saveIndex(ctx context.Context, id string) (*save.Index, error) {
	_, m, err := e.verifiedSave(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Base == nil {
		f, err := os.Open(filepath.Join(e.saveDir(id), save.RootfsFile))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return save.BuildIndex(f)
	}
	base, err := e.baseIndex(m.Base.SHA256)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(e.saveDir(id), save.DeltaFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return save.IndexWithDelta(base, f)
}
