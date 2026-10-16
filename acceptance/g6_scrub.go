package acceptance

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AQLD/cairn/internal/save"
)

// G6.10: damaged saves are found by scrubbing before anyone restores them.

func init() {
	register("G6.10.1", func(e *Env) {
		ws := e.Workspace("scrub")
		e.Write(ws.ID, "f", "x")
		good := e.Save(ws.ID)
		bad := e.Save(ws.ID)
		path := filepath.Join(e.Engine.SaveDir(bad.ID), save.RootfsFile)
		data, err := os.ReadFile(path)
		e.Must(err)
		data[len(data)/3] ^= 0x55
		e.Must(os.WriteFile(path, data, 0o600))

		checked, notReady, err := e.Engine.Scrub(e.Ctx)
		e.Must(err)
		Equal(e, "checked", checked, 2)
		Equal(e, "not ready", notReady, 1)

		got, err := e.C.GetSave(e.Ctx, bad.ID)
		e.Must(err)
		Equal(e, "damaged save", got.State, "corrupt")
		got, err = e.C.GetSave(e.Ctx, good.ID)
		e.Must(err)
		Equal(e, "good save", got.State, "ready")

		_, err = e.C.Resume(e.Ctx, bad.ID)
		e.ErrCode(err, "save_corrupt")
		text, err := e.C.Metrics(e.Ctx)
		e.Must(err)
		if !containsLine(text, `cairn_saves{state="corrupt"} 1`) {
			e.T.Fatalf("metrics do not show the corrupt save:\n%s", text)
		}
	})

	register("G6.10.2", func(e *Env) {
		ws := e.Workspace("verify")
		sv := e.Save(ws.ID)
		got, err := e.C.VerifySave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "fresh save", got.State, "ready")

		// damage it: verify says so
		path := filepath.Join(e.Engine.SaveDir(sv.ID), save.RootfsFile)
		orig, err := os.ReadFile(path)
		e.Must(err)
		e.Must(os.WriteFile(path, append([]byte("x"), orig...), 0o600))
		got, err = e.C.VerifySave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "damaged", got.State, "corrupt")

		// repair it (say, from a backup): verify brings it back
		e.Must(os.WriteFile(path, orig, 0o600))
		got, err = e.C.VerifySave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "repaired", got.State, "ready")
		e.Resume(sv.ID)

		// gone entirely
		e.Must(os.RemoveAll(e.Engine.SaveDir(sv.ID)))
		got, err = e.C.VerifySave(e.Ctx, sv.ID)
		e.Must(err)
		Equal(e, "missing", got.State, "missing")
	})
}

func containsLine(text, line string) bool {
	return slices.Contains(strings.Split(text, "\n"), line)
}
