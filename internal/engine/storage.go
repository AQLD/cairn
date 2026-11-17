package engine

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"syscall"
)

// Storage is what the engine's data uses on disk: the numbers a laptop
// owner needs before the volume fills (dogfooding filled 31 GB in an
// evening). Bytes are allocated blocks, not apparent sizes, so a sparse
// 10 GB workspace image that holds 300 MB counts as 300 MB.
type Storage struct {
	VolumeFree   int64            `json:"volume_free_bytes"`
	VolumeTotal  int64            `json:"volume_total_bytes"`
	Workspaces   int64            `json:"workspaces_bytes"`
	Saves        int64            `json:"saves_bytes"`
	Templates    int64            `json:"templates_bytes"`
	PerWorkspace []WorkspaceUsage `json:"per_workspace"`
}

// WorkspaceUsage is one workspace's share, largest first.
type WorkspaceUsage struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Bytes int64  `json:"bytes"`
}

// Storage measures the data folder. It walks every workspace, so it is for
// people and dashboards, not for every request.
func (e *Engine) Storage(ctx context.Context) (*Storage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(e.cfg.DataDir, &st); err != nil {
		return nil, err
	}
	out := &Storage{
		VolumeFree:  int64(st.Bavail) * int64(st.Bsize), //nolint:unconvert // field types differ by OS
		VolumeTotal: int64(st.Blocks) * int64(st.Bsize), //nolint:unconvert
		Saves:       allocated(filepath.Join(e.cfg.DataDir, "saves")),
		Templates:   allocated(e.templatesDir()),
	}
	ws, err := e.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		n := allocated(e.workspaceDir(w.ID))
		out.Workspaces += n
		out.PerWorkspace = append(out.PerWorkspace, WorkspaceUsage{ID: w.ID, Name: w.Name, State: string(w.State), Bytes: n})
	}
	sort.Slice(out.PerWorkspace, func(i, j int) bool { return out.PerWorkspace[i].Bytes > out.PerWorkspace[j].Bytes })
	return out, nil
}

// allocated is the space the files under root take on disk. Mounted
// filesystems inside (a workspace's loop-mounted image) are not entered:
// the image file itself is counted.
func allocated(root string) int64 {
	var rootDev uint64
	if fi, err := lstatSys(root); err == nil {
		rootDev = uint64(fi.Dev) //nolint:unconvert
	} else {
		return 0
	}
	var n int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		s, err := lstatSys(path)
		if err != nil {
			return nil
		}
		if d.IsDir() && uint64(s.Dev) != rootDev { //nolint:unconvert
			return filepath.SkipDir
		}
		n += int64(s.Blocks) * 512 //nolint:unconvert
		return nil
	})
	return n
}

func lstatSys(path string) (*syscall.Stat_t, error) {
	var s syscall.Stat_t
	if err := syscall.Lstat(path, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
