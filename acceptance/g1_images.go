package acceptance

import (
	"archive/tar"
	"strings"

	"github.com/AQLD/cairn/client"
	"github.com/AQLD/cairn/internal/oci/ocitest"
)

func init() {
	// G1.10.5: a team's own image, in its private registry, becomes a template.
	// The registry runs in the test process (hence in-process only) and asks
	// for a login, as a private one would.
	register("G1.10.5", func(e *Env) {
		reg := ocitest.New()
		defer reg.Close()
		reg.Auth, reg.Username, reg.Password = ocitest.Token, "ci-robot", "registry-secret"
		base := ocitest.Layer(
			ocitest.Entry{Name: "etc/", Type: tar.TypeDir},
			ocitest.Entry{Name: "etc/team-release", Body: "base\n"},
			ocitest.Entry{Name: "etc/stale.conf", Body: "removed by the next layer\n"},
			ocitest.Entry{Name: "opt/tools/old", Body: "hidden by the next layer\n"},
			ocitest.Entry{Name: "usr/bin/team-tool", Body: "#!/bin/sh\necho tool\n", Mode: 0o755},
		)
		top := ocitest.Layer(
			ocitest.Entry{Name: "etc/team-release", Body: "v1\n"},
			ocitest.Entry{Name: "etc/stale.conf", Whiteout: true},
			ocitest.Entry{Name: "opt/tools", Opaque: true},
			ocitest.Entry{Name: "opt/tools/new", Body: "from the top layer\n"},
		)
		reg.Push("team/agent-base", "v1", true, base, top)
		image := reg.Host() + "/team/agent-base:v1"
		pull := func(img, password string) (*client.Template, error) {
			return e.C.PullTemplate(e.Ctx, "team-base", client.PullTemplate{
				Image: img, Username: "ci-robot", Password: password, Insecure: true})
		}

		_, err := pull(image, "wrong")
		e.ErrCode(err, "invalid_argument")
		_, err = pull(reg.Host()+"/team/agent-base:missing", "registry-secret")
		e.ErrCode(err, "not_found")

		t, err := pull(image, "registry-secret")
		e.Must(err)
		Equal(e, "template records its image", t.Image, image)
		if !strings.HasPrefix(t.ImageDigest, "sha256:") {
			e.T.Fatalf("image digest not recorded: %q", t.ImageDigest)
		}

		ws, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "from-image", Template: "team-base"})
		e.Must(err)
		Equal(e, "upper layer wins", e.Read(ws.ID, "etc/team-release"), "v1\n")
		Equal(e, "lower layer kept", e.Read(ws.ID, "usr/bin/team-tool"), "#!/bin/sh\necho tool\n")
		Equal(e, "opaque folder holds only the upper layer", e.Read(ws.ID, "opt/tools/new"), "from the top layer\n")
		for _, deleted := range []string{"etc/stale.conf", "opt/tools/old"} {
			_, err := e.C.ReadFile(e.Ctx, ws.ID, deleted)
			e.ErrCode(err, "not_found")
		}

		// a newer image under the same name: new workspaces get it, existing
		// ones keep what they started with
		reg.Push("team/agent-base", "v2", true, base, ocitest.Layer(ocitest.Entry{Name: "etc/team-release", Body: "v2\n"}))
		_, err = pull(reg.Host()+"/team/agent-base:v2", "registry-secret")
		e.Must(err)
		ws2, err := e.C.CreateWorkspace(e.Ctx, client.CreateWorkspace{Name: "from-image-v2", Template: "team-base"})
		e.Must(err)
		Equal(e, "new workspace gets v2", e.Read(ws2.ID, "etc/team-release"), "v2\n")
		Equal(e, "existing workspace keeps v1", e.Read(ws.ID, "etc/team-release"), "v1\n")
	})
}
