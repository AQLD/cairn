package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"

	"github.com/AQLD/cairn/internal/save"
)

// ArchiveStats counts what a push or pull moved.
type ArchiveStats = save.ArchiveStats

// PushOptions tune PushDir.
type PushOptions struct {
	// Exclude leaves out entries whose name or slash-separated path relative to
	// the folder matches one of these path.Match patterns, such as
	// "node_modules", ".venv" or "*.pyc". A matching folder is left out whole.
	// Nothing is excluded by default (.git included: agents need history).
	Exclude []string
}

func archivePath(id, dir string) string {
	return "/v1/workspaces/" + url.PathEscape(id) + "/archive?" + url.Values{"path": {dir}}.Encode()
}

// PushArchive merges a tar stream into folder dir of the workspace, which is
// created if missing. A file of the same name is replaced; nothing else in dir
// is removed.
func (c *Client) PushArchive(ctx context.Context, id, dir string, tarStream io.Reader) (*ArchiveStats, error) {
	var buf bytes.Buffer
	if err := c.doRaw(ctx, http.MethodPut, archivePath(id, dir), tarStream, &buf); err != nil {
		return nil, err
	}
	var st ArchiveStats
	return &st, json.Unmarshal(buf.Bytes(), &st)
}

// PullArchive writes folder dir of the workspace to w as a tar stream.
func (c *Client) PullArchive(ctx context.Context, id, dir string, w io.Writer) error {
	return c.doRaw(ctx, http.MethodGet, archivePath(id, dir), nil, w)
}

// PushDir copies a local folder into folder remoteDir of the workspace,
// merging with what is there: modes, times, symlinks and empty folders are
// kept, ownership is not.
func (c *Client) PushDir(ctx context.Context, id, localDir, remoteDir string, opt PushOptions) (*ArchiveStats, error) {
	for _, p := range opt.Exclude {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("exclude pattern %q: %w", p, err)
		}
	}
	root, err := os.OpenRoot(localDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	skip := func(name string, _ fs.DirEntry) bool {
		for _, p := range opt.Exclude {
			if ok, _ := path.Match(p, path.Base(name)); ok {
				return true
			}
			if ok, _ := path.Match(p, name); ok {
				return true
			}
		}
		return false
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := save.TarRoot(root, pw, skip)
		pw.CloseWithError(err)
	}()
	st, err := c.PushArchive(ctx, id, remoteDir, pr)
	pr.CloseWithError(io.ErrClosedPipe) // the engine may answer before reading it all
	return st, err
}

// PullDir copies folder remoteDir of the workspace into a local folder
// (created if missing), merging with what is there. Nothing it extracts can
// land outside localDir.
func (c *Client) PullDir(ctx context.Context, id, remoteDir, localDir string) (*ArchiveStats, error) {
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(localDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := c.PullArchive(ctx, id, remoteDir, pw)
		pw.CloseWithError(err)
		errc <- err
	}()
	st, err := save.ExtractTo(root, pr, 1<<62)
	pr.CloseWithError(io.ErrClosedPipe)
	if rerr := <-errc; rerr != nil && rerr != io.ErrClosedPipe {
		return &st, rerr // the engine's error explains a broken stream best
	}
	return &st, err
}
