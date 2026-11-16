package acceptance

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AQLD/cairn/client"
)

// entry is one thing in a folder tree, as compared after a round trip.
type entry struct {
	kind  string // file, dir, link
	mode  fs.FileMode
	data  string // file content or link target
	mtime int64
}

// tree describes a local folder: every entry by slash-separated path.
func tree(e *Env, dir string) map[string]entry {
	e.T.Helper()
	out := map[string]entry{}
	e.Must(filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		en := entry{mode: info.Mode().Perm(), mtime: info.ModTime().Unix()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			en.kind, en.mode, en.mtime = "link", 0, 0 // a link's own mode and time are not portable
			en.data, err = os.Readlink(p)
		case info.IsDir():
			en.kind = "dir"
		default:
			en.kind = "file"
			var b []byte
			b, err = os.ReadFile(p)
			en.data = string(b)
		}
		out[filepath.ToSlash(rel)] = en
		return err
	}))
	return out
}

// project writes a small but awkward project folder: modes, an empty folder,
// symlinks, binary content, odd names, git metadata and things to exclude.
func project(e *Env) string {
	e.T.Helper()
	dir := filepath.Join(e.T.TempDir(), "proj")
	bin := make([]byte, 300<<10)
	_, _ = rand.Read(bin)
	files := []struct {
		name string
		mode fs.FileMode
		data string
	}{
		{"run.sh", 0o755, "#!/bin/sh\necho ran-from-push\n"},
		{"secret.env", 0o600, "TOKEN=x\n"},
		{"src/main.py", 0o644, "print('hi')\n"},
		{"src/deep/a b ü.txt", 0o640, "odd name\n"},
		{"data.bin", 0o644, string(bin)},
		{".git/HEAD", 0o644, "ref: refs/heads/main\n"},
		{"node_modules/left/index.js", 0o644, "module.exports = 1\n"},
		{"src/cache.pyc", 0o644, "junk"},
	}
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.name))
		e.Must(os.MkdirAll(filepath.Dir(p), 0o755))
		e.Must(os.WriteFile(p, []byte(f.data), f.mode))
		e.Must(os.Chmod(p, f.mode)) // regardless of umask
		e.Must(os.Chtimes(p, when, when))
	}
	e.Must(os.MkdirAll(filepath.Join(dir, "empty", "nested"), 0o750))
	e.Must(os.Chmod(filepath.Join(dir, "empty"), 0o700))
	e.Must(os.Symlink("src/main.py", filepath.Join(dir, "main-link")))
	e.Must(os.Symlink("../run.sh", filepath.Join(dir, "src", "up-link")))
	e.Must(os.Symlink("does-not-exist", filepath.Join(dir, "dangling")))
	for _, d := range []string{"src/deep", "src", "empty/nested", "empty", ".git", "node_modules/left", "node_modules", "."} {
		e.Must(os.Chtimes(filepath.Join(dir, d), when, when))
	}
	return dir
}

func init() {
	// G1.14: a project folder goes in and comes back unchanged.
	register("G1.14", func(e *Env) {
		ws := e.Workspace("project")
		local := project(e)
		st, err := e.C.PushDir(e.Ctx, ws.ID, local, "work/proj", client.PushOptions{Exclude: []string{"node_modules", "*.pyc"}})
		e.Must(err)
		Equal(e, "files pushed", st.Files, 6)
		Equal(e, "links pushed", st.Links, 3)

		// the workspace's programs see the files, executable bits included
		Equal(e, "pushed script runs", strings.TrimSpace(e.Sh(ws.ID, "cd work/proj && ./run.sh && ./src/up-link")), "ran-from-push\nran-from-push")
		Equal(e, "content through a link", e.Read(ws.ID, "work/proj/main-link"), "print('hi')\n")

		back := filepath.Join(e.T.TempDir(), "back")
		_, err = e.C.PullDir(e.Ctx, ws.ID, "work/proj", back)
		e.Must(err)
		want := tree(e, local)
		for name := range want {
			if strings.HasPrefix(name, "node_modules") || strings.HasSuffix(name, ".pyc") {
				delete(want, name)
			}
		}
		got := tree(e, back)
		for name, w := range want {
			g, ok := got[name]
			switch {
			case !ok:
				e.T.Errorf("%s: missing after the round trip", name)
			case g.kind != w.kind || g.mode != w.mode || g.data != w.data:
				e.T.Errorf("%s: came back as %s %o (%d bytes), want %s %o (%d bytes)", name, g.kind, g.mode, len(g.data), w.kind, w.mode, len(w.data))
			case g.mtime != w.mtime:
				e.T.Errorf("%s: modified time %d, want %d", name, g.mtime, w.mtime)
			}
		}
		for name := range got {
			if _, ok := want[name]; !ok {
				e.T.Errorf("%s: came back but was never pushed (excluded or invented)", name)
			}
		}

		// a second push merges: changed files are replaced, files only the
		// workspace has stay, and a file that became a link is replaced too
		e.Write(ws.ID, "work/proj/made-inside.txt", "agent's own")
		e.Must(os.WriteFile(filepath.Join(local, "src", "main.py"), []byte("print('v2')\n"), 0o644))
		e.Must(os.Remove(filepath.Join(local, "secret.env")))
		e.Must(os.Symlink("run.sh", filepath.Join(local, "secret.env")))
		_, err = e.C.PushDir(e.Ctx, ws.ID, local, "work/proj", client.PushOptions{})
		e.Must(err)
		Equal(e, "replaced file", e.Read(ws.ID, "work/proj/src/main.py"), "print('v2')\n")
		Equal(e, "workspace's own file kept", e.Read(ws.ID, "work/proj/made-inside.txt"), "agent's own")
		Equal(e, "file replaced by a link", e.Read(ws.ID, "work/proj/secret.env"), "#!/bin/sh\necho ran-from-push\n")
		Equal(e, "excluded folder pushed without exclude", e.Read(ws.ID, "work/proj/node_modules/left/index.js"), "module.exports = 1\n")

		// what is not a folder is refused with stable codes
		_, err = e.C.PullDir(e.Ctx, ws.ID, "work/missing", filepath.Join(e.T.TempDir(), "x"))
		e.ErrCode(err, "not_found")
		_, err = e.C.PullDir(e.Ctx, ws.ID, "work/proj/run.sh", filepath.Join(e.T.TempDir(), "y"))
		e.ErrCode(err, "invalid_argument")
		_, err = e.C.PushArchive(e.Ctx, ws.ID, "work/junk", strings.NewReader(strings.Repeat("not a tar archive ", 100)))
		e.ErrCode(err, "invalid_argument")
	})

	// G8.8: folder archives cannot reach outside the workspace.
	register("G8.8", func(e *Env) {
		ws := e.Workspace("archive-escape")
		host := e.T.TempDir() // stands for anything on the host
		e.Must(os.WriteFile(filepath.Join(host, "secret.txt"), []byte("host secret"), 0o600))

		mk := func(entries ...func(*tar.Writer)) *bytes.Reader {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			for _, f := range entries {
				f(tw)
			}
			e.Must(tw.Close())
			return bytes.NewReader(buf.Bytes())
		}
		file := func(name, body string) func(*tar.Writer) {
			return func(tw *tar.Writer) {
				e.Must(tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
				_, err := tw.Write([]byte(body))
				e.Must(err)
			}
		}
		link := func(name, target string, kind byte) func(*tar.Writer) {
			return func(tw *tar.Writer) {
				e.Must(tw.WriteHeader(&tar.Header{Name: name, Linkname: target, Mode: 0o777, Typeflag: kind}))
			}
		}
		climb := strings.Repeat("../", 12) + strings.TrimPrefix(host, "/") + "/pwned"
		attacks := map[string]*bytes.Reader{
			"climbing path":            mk(file("../../../../pwned", "x")),
			"climbing to the host":     mk(file(climb, "x")),
			"absolute path":            mk(file(host+"/pwned", "x")),
			"hard link out":            mk(link("hl", climb, tar.TypeLink)),
			"hard link to host secret": mk(link("hl2", host+"/secret.txt", tar.TypeLink)),
			"write through a symlink":  mk(link("esc", host, tar.TypeSymlink), file("esc/pwned", "x")),
			"folder through a symlink": mk(link("esc2", host, tar.TypeSymlink), file("esc2/sub/pwned", "x")),
		}
		for what, tarball := range attacks {
			_, err := e.C.PushArchive(e.Ctx, ws.ID, "box/"+strings.ReplaceAll(what, " ", "-"), tarball)
			e.ErrCode(err, "invalid_argument")
			if _, err := os.Stat(filepath.Join(host, "pwned")); err == nil {
				e.T.Fatalf("%s: a pushed archive wrote a file on the host", what)
			}
			if _, err := os.Stat(filepath.Join(host, "sub")); err == nil {
				e.T.Fatalf("%s: a pushed archive made a folder on the host", what)
			}
		}
		if e.DataDir != "" {
			if _, err := os.Stat(filepath.Join(e.DataDir, "pwned")); err == nil {
				e.T.Fatal("a pushed archive wrote next to the workspaces")
			}
		}

		// the target folder itself is a link out: pushing into it is refused
		e.Sh(ws.ID, "mkdir -p trap && ln -s "+host+" trap/out")
		_, err := e.C.PushArchive(e.Ctx, ws.ID, "trap/out", mk(file("pwned", "x")))
		e.ErrCode(err, "invalid_argument")
		_, err = e.C.PushArchive(e.Ctx, ws.ID, "trap", mk(file("out/pwned", "x")))
		e.ErrCode(err, "invalid_argument")
		if _, err := os.Stat(filepath.Join(host, "pwned")); err == nil {
			e.T.Fatal("a push through a planted link wrote on the host")
		}

		// pulling through a planted link never reads host files
		_, err = e.C.PullDir(e.Ctx, ws.ID, "trap/out", filepath.Join(e.T.TempDir(), "a"))
		if err == nil {
			e.T.Fatal("pulling a link that leaves the workspace succeeded")
		}
		var buf bytes.Buffer
		e.Must(e.C.PullArchive(e.Ctx, ws.ID, "trap", &buf))
		if bytes.Contains(buf.Bytes(), []byte("host secret")) {
			e.T.Fatal("a pulled folder carried a host file through a link")
		}
		tr := tar.NewReader(&buf)
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			if hdr.Name == "out" && hdr.Typeflag != tar.TypeSymlink {
				e.T.Fatalf("the planted link came back as type %c, not as a link", hdr.Typeflag)
			}
		}
	})
}
