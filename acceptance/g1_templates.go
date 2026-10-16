package acceptance

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/save"
)

// G1.10: workspaces start from a ready toolchain.

// templateTar builds a root filesystem tar from files. On runtimes that boot a
// real sandbox, the files go on top of CAIRN_TEST_TEMPLATE, which has a shell.
func templateTar(e *Env, files map[string]string) *bytes.Buffer {
	e.T.Helper()
	dir := e.T.TempDir()
	if base := os.Getenv("CAIRN_TEST_TEMPLATE"); base != "" {
		f, err := os.Open(base)
		e.Must(err)
		info, err := f.Stat()
		e.Must(err)
		e.Must(save.UntarDir(f, dir, info.Size()))
		f.Close()
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		e.Must(os.MkdirAll(filepath.Dir(p), 0o755))
		e.Must(os.WriteFile(p, []byte(content), 0o755))
	}
	var buf bytes.Buffer
	e.Must(save.TarDir(dir, &buf))
	return &buf
}

func init() {
	register("G1.10.1", func(e *Env) {
		existing, err := e.C.ListTemplates(e.Ctx)
		e.Must(err)
		_, err = e.C.ImportTemplate(e.Ctx, "toolbox", templateTar(e, map[string]string{
			"usr/local/bin/hello": "#!/bin/sh\necho hello from the template\n",
			"etc/motd":            "welcome\n",
		}))
		e.Must(err)
		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "toolbox"})
		e.Must(err)
		Equal(e, "template recorded", ws.Template, "toolbox")
		Equal(e, "file from template", e.Read(ws.ID, "etc/motd"), "welcome\n")
		Equal(e, "tool from template runs", e.Sh(ws.ID, "sh usr/local/bin/hello"), "hello from the template\n")
		list, err := e.C.ListTemplates(e.Ctx)
		e.Must(err)
		Equal(e, "templates", len(list), len(existing)+1)
	})

	register("G1.10.2", func(e *Env) {
		_, err := e.C.ImportTemplate(e.Ctx, "base", templateTar(e, map[string]string{"shared.txt": "original"}))
		e.Must(err)
		a, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "base"})
		e.Must(err)
		b, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "base"})
		e.Must(err)
		e.Write(a.ID, "shared.txt", "changed in a")
		Equal(e, "b untouched", e.Read(b.ID, "shared.txt"), "original")
		c, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "base"})
		e.Must(err)
		Equal(e, "template untouched", e.Read(c.ID, "shared.txt"), "original")
	})

	register("G1.10.3", func(e *Env) {
		existing, err := e.C.ListTemplates(e.Ctx)
		e.Must(err)
		_, err = e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "nope"})
		e.ErrCode(err, "not_found")
		_, err = e.C.ImportTemplate(e.Ctx, "broken", bytes.NewReader([]byte("this is not a tar file at all, just text")))
		e.ErrCode(err, "invalid_argument")
		_, err = e.C.ImportTemplate(e.Ctx, "Bad Name", templateTar(e, map[string]string{"a": "b"}))
		e.ErrCode(err, "invalid_argument")
		list, err := e.C.ListTemplates(e.Ctx)
		e.Must(err)
		Equal(e, "nothing imported", len(list), len(existing))
		ws, err := e.C.ListWorkspaces(e.Ctx)
		e.Must(err)
		Equal(e, "no workspace left behind", len(ws), 0)
	})

	register("G1.10.4", func(e *Env) {
		_, err := e.C.ImportTemplate(e.Ctx, "v", templateTar(e, map[string]string{"version": "1"}))
		e.Must(err)
		old, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "v"})
		e.Must(err)
		_, err = e.C.ImportTemplate(e.Ctx, "v", templateTar(e, map[string]string{"version": "2"}))
		e.Must(err)
		fresh, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Template: "v"})
		e.Must(err)
		Equal(e, "existing workspace keeps v1", e.Read(old.ID, "version"), "1")
		Equal(e, "new workspace gets v2", e.Read(fresh.ID, "version"), "2")
	})
}
