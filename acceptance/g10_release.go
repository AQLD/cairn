package acceptance

import (
	"archive/tar"
	"compress/gzip"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// G10.8: signed releases with a bill of materials.

// untar extracts a .tar.gz into dir.
func untar(e *Env, archive, dir string) {
	e.T.Helper()
	f, err := os.Open(archive)
	e.Must(err)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	e.Must(err)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		e.Must(err)
		p := filepath.Join(dir, filepath.Clean("/"+h.Name))
		if h.Typeflag == tar.TypeDir {
			e.Must(os.MkdirAll(p, 0o755))
			continue
		}
		e.Must(os.MkdirAll(filepath.Dir(p), 0o755))
		out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, os.FileMode(h.Mode))
		e.Must(err)
		_, err = io.Copy(out, tr)
		e.Must(err)
		e.Must(out.Close())
	}
}

// fakeCosign is a stand-in cosign for the installer's logic: verify-blob
// succeeds only when the bundle holds SHA256SUMS's digest (what "signed" means
// here); real signatures are G10.8.3's.
const fakeCosign = `#!/bin/sh
[ "$1" = verify-blob ] || exit 2
blob= bundle=
while [ $# -gt 0 ]; do
	case $1 in --bundle) bundle=$2; shift ;; --*=*) ;; --key|--certificate-identity|--certificate-oidc-issuer) shift ;; verify-blob) ;; *) blob=$1 ;; esac
	shift
done
want=$(cat "$bundle")
if command -v sha256sum >/dev/null; then got=$(sha256sum "$blob" | cut -d' ' -f1); else got=$(shasum -a 256 "$blob" | cut -d' ' -f1); fi
[ "$got" = "$want" ]
`

func init() {
	register("G10.8.1", func(e *Env) {
		out := e.T.TempDir()
		// the Linux archive (cross-built anywhere): it has cairnd, which links
		// modules; the CLI alone links only the standard library
		target := "linux/" + runtime.GOARCH
		cmd := exec.CommandContext(e.Ctx, "bash", "../deploy/release.sh", "v0.0.0-test", out)
		cmd.Env = append(os.Environ(), "CAIRN_RELEASE_TARGETS="+target)
		if o, err := cmd.CombinedOutput(); err != nil {
			e.T.Fatalf("release.sh: %v\n%s", err, o)
		}
		name := fmt.Sprintf("cairn_v0.0.0-test_linux_%s", runtime.GOARCH)
		// SHA256SUMS covers the archive and its bill of materials, and they match
		sums, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
		e.Must(err)
		for _, f := range []string{name + ".tar.gz", name + ".cdx.json"} {
			want := sha256File(e, filepath.Join(out, f))
			if !strings.Contains(string(sums), want+"  "+f+"\n") {
				e.T.Fatalf("SHA256SUMS does not hold %s's digest:\n%s", f, sums)
			}
		}
		// the bill of materials lists exactly what the binaries link
		x := e.T.TempDir()
		untar(e, filepath.Join(out, name+".tar.gz"), x)
		linked := map[string]string{}
		bins := []string{"cairn", "cairnd", "cairn-init"}
		goVersion := ""
		for _, b := range bins {
			bi, err := buildinfo.ReadFile(filepath.Join(x, name, b))
			e.Must(err)
			goVersion = bi.GoVersion
			for _, m := range bi.Deps {
				if m.Replace != nil {
					m = m.Replace
				}
				linked[m.Path] = m.Version
			}
		}
		if len(linked) < 2 {
			e.T.Fatalf("the binaries link no modules: the check would prove nothing")
		}
		linked["stdlib"] = goVersion
		var bom struct {
			BOMFormat  string `json:"bomFormat"`
			Components []struct {
				Type, Name, Version, PURL string
			} `json:"components"`
		}
		data, err := os.ReadFile(filepath.Join(out, name+".cdx.json"))
		e.Must(err)
		e.Must(json.Unmarshal(data, &bom))
		Equal(e, "format", bom.BOMFormat, "CycloneDX")
		listed := map[string]string{}
		for _, c := range bom.Components {
			if c.Type == "library" {
				listed[c.Name] = c.Version
			}
		}
		keys := func(m map[string]string) string {
			var k []string
			for n, v := range m {
				k = append(k, n+"@"+v)
			}
			sort.Strings(k)
			return strings.Join(k, " ")
		}
		Equal(e, "modules in the bill of materials", keys(listed), keys(linked))
	})

	register("G10.8.2", func(e *Env) {
		root := e.T.TempDir()
		srv := httptest.NewServer(http.FileServer(http.Dir(root)))
		defer srv.Close()
		fake := e.T.TempDir()
		e.Must(os.WriteFile(filepath.Join(fake, "cosign"), []byte(fakeCosign), 0o755))
		dest := filepath.Join(e.T.TempDir(), "bin")
		install := func(version string, withCosign bool, extra ...string) (string, error) {
			path := withoutCommand("cosign")
			if withCosign {
				path = fake + string(os.PathListSeparator) + path
			}
			cmd := exec.CommandContext(e.Ctx, "sh", "../install.sh")
			cmd.Env = append(append(envWithPath(path), "CAIRN_VERSION="+version, "CAIRN_DOWNLOAD_BASE="+srv.URL+"/"+version,
				"CAIRN_INSTALL="+dest, "HOME="+e.T.TempDir()), extra...)
			out, err := cmd.CombinedOutput()
			return string(out), err
		}
		sign := func(dir string) {
			e.Must(os.WriteFile(filepath.Join(dir, "SHA256SUMS.sigstore.json"), []byte(sha256File(e, filepath.Join(dir, "SHA256SUMS"))), 0o644))
		}

		// a signed release installs, and says it checked
		good := filepath.Join(root, "v0.9.1-test")
		fakeRelease(e, good, "v0.9.1-test")
		sign(good)
		if out, err := install("v0.9.1-test", true); err != nil || !strings.Contains(out, "signature checked") {
			e.T.Fatalf("signed release: %v\n%s", err, out)
		}
		// archive and SHA256SUMS replaced together (the checksum then matches):
		// the signature no longer does
		evil := filepath.Join(root, "v0.9.2-test")
		fakeRelease(e, evil, "v0.9.2-test")
		sign(evil)
		fakeRelease(e, evil, "v0.9.2-test") // rebuilt: new archive, new SHA256SUMS, old signature
		e.Must(os.WriteFile(filepath.Join(evil, "SHA256SUMS"), append(mustRead(e, filepath.Join(evil, "SHA256SUMS")), []byte("# replaced\n")...), 0o644))
		if out, err := install("v0.9.2-test", true); err == nil || !strings.Contains(out, "signature does not verify") {
			e.T.Fatalf("replaced SHA256SUMS: %v\n%s", err, out)
		}
		// a release stripped of its signature
		stripped := filepath.Join(root, "v0.9.3-test")
		fakeRelease(e, stripped, "v0.9.3-test")
		if out, err := install("v0.9.3-test", true); err == nil || !strings.Contains(out, "has no signature") {
			e.T.Fatalf("stripped signature: %v\n%s", err, out)
		}
		// requiring a signature without cosign
		if out, err := install("v0.9.1-test", false, "CAIRN_REQUIRE_SIGNATURE=1"); err == nil || !strings.Contains(out, "cosign is not installed") {
			e.T.Fatalf("required signature without cosign: %v\n%s", err, out)
		}
		// what is installed is still the good one
		out, err := exec.Command(filepath.Join(dest, "cairn"), "version").Output()
		e.Must(err)
		Equal(e, "installed version", strings.TrimSpace(string(out)), "v0.9.1-test")
	})

	register("G10.8.3", func(e *Env) {
		version := os.Getenv("CAIRN_TEST_RELEASE")
		if version == "" {
			e.T.Skip("set CAIRN_TEST_RELEASE to a published signed release (v0.3.0 or later) to verify it")
		}
		if _, err := exec.LookPath("cosign"); err != nil {
			e.T.Skip("needs cosign")
		}
		dir := e.T.TempDir()
		base := "https://github.com/AQLD/cairn/releases/download/" + version + "/"
		for _, f := range []string{"SHA256SUMS", "SHA256SUMS.sigstore.json"} {
			run(e, "curl", "-fsSL", "-o", filepath.Join(dir, f), base+f)
		}
		run(e, "cosign", "verify-blob", "--bundle", filepath.Join(dir, "SHA256SUMS.sigstore.json"),
			"--certificate-identity", "https://github.com/AQLD/cairn/.github/workflows/release.yml@refs/tags/"+version,
			"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", filepath.Join(dir, "SHA256SUMS"))
		for _, line := range strings.Split(strings.TrimSpace(string(mustRead(e, filepath.Join(dir, "SHA256SUMS")))), "\n") {
			f := strings.Fields(line)
			run(e, "curl", "-fsSL", "-o", filepath.Join(dir, f[1]), base+f[1])
			Equal(e, f[1], sha256File(e, filepath.Join(dir, f[1])), f[0])
		}
	})
}

func mustRead(e *Env, p string) []byte {
	e.T.Helper()
	b, err := os.ReadFile(p)
	e.Must(err)
	return b
}
