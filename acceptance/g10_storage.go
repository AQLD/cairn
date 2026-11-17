package acceptance

import (
	"strings"

	"github.com/AQLD/cairn/client"
)

func init() {
	register("G10.16", func(e *Env) {
		big, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "big", Limits: client.Limits{DiskMB: 10240}})
		e.Must(err)
		small := e.Workspace("small")
		// 30 MB of data that does not compress or dedupe away
		e.Sh(big.ID, "head -c 31457280 /dev/urandom > data.bin")

		st, err := e.C.Storage(e.Ctx)
		e.Must(err)
		if st.VolumeTotal <= 0 || st.VolumeFree <= 0 || st.VolumeFree > st.VolumeTotal {
			e.T.Fatalf("volume: %d free of %d", st.VolumeFree, st.VolumeTotal)
		}
		if len(st.PerWorkspace) < 2 || st.PerWorkspace[0].ID != big.ID {
			e.T.Fatalf("per workspace, largest first: %+v", st.PerWorkspace)
		}
		b := st.PerWorkspace[0].Bytes
		if b < 30<<20 {
			e.T.Fatalf("workspace holding 30 MB counted as %d bytes", b)
		}
		// allocated, not apparent: the 10 GB disk is sparse
		if b > 2<<30 {
			e.T.Fatalf("workspace holding 30 MB counted as %d bytes: apparent size, not allocated", b)
		}
		var sum int64
		for _, w := range st.PerWorkspace {
			sum += w.Bytes
			if w.ID == small.ID && w.Name != "small" {
				e.T.Fatalf("names: %+v", w)
			}
		}
		Equal(e, "workspaces total", st.Workspaces, sum)

		savesBefore := st.Saves
		e.Save(big.ID)
		st, err = e.C.Storage(e.Ctx)
		e.Must(err)
		if st.Saves-savesBefore < 30<<20 {
			e.T.Fatalf("a save of 30 MB added %d bytes to saves", st.Saves-savesBefore)
		}

		// a copy shows up, named as a copy
		sv := e.Save(small.ID)
		ws, err := e.C.Copy(e.Ctx, sv.ID, 1)
		e.Must(err)
		st, err = e.C.Storage(e.Ctx)
		e.Must(err)
		found := false
		for _, w := range st.PerWorkspace {
			if w.ID == ws[0].ID && strings.Contains(w.Name, "-copy-") {
				found = true
			}
		}
		if !found {
			e.T.Fatalf("copy %s not listed as a copy: %+v", ws[0].ID, st.PerWorkspace)
		}
	})
}
