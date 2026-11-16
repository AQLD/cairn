package acceptance

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// fakeRelease builds what release.yml publishes for this machine (a
// tar.gz with cairn inside, and SHA256SUMS) into a folder.
func fakeRelease(e *Env, dir, version string) {
	e.T.Helper()
	name := fmt.Sprintf("cairn_%s_%s_%s", version, runtime.GOOS, runtime.GOARCH)
	bin := filepath.Join(e.T.TempDir(), "cairn")
	if out, err := exec.CommandContext(e.Ctx, "go", "build", "-ldflags", "-X main.buildVersion="+version, "-o", bin, "../cmd/cairn").CombinedOutput(); err != nil {
		e.T.Fatalf("building cairn: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bin)
	e.Must(err)
	e.Must(os.MkdirAll(dir, 0o755))
	f, err := os.Create(filepath.Join(dir, name+".tar.gz"))
	e.Must(err)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	e.Must(tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}))
	e.Must(tw.WriteHeader(&tar.Header{Name: name + "/cairn", Mode: 0o755, Size: int64(len(data))}))
	_, err = tw.Write(data)
	e.Must(err)
	e.Must(tw.Close())
	e.Must(gz.Close())
	e.Must(f.Close())
	sum := sha256File(e, filepath.Join(dir, name+".tar.gz"))
	// as sha256sum writes it, with a decoy for another platform
	sums := sum + "  " + name + ".tar.gz\n" + strings.Repeat("0", 64) + "  cairn_" + version + "_plan9_mips.tar.gz\n"
	e.Must(os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o644))
}

func sha256File(e *Env, path string) string {
	data, err := os.ReadFile(path)
	e.Must(err)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func init() {
	register("G10.15", func(e *Env) {
		root := e.T.TempDir()
		srv := httptest.NewServer(http.FileServer(http.Dir(root)))
		defer srv.Close()
		dest := filepath.Join(e.T.TempDir(), "bin")
		install := func(version string) (string, error) {
			cmd := exec.CommandContext(e.Ctx, "sh", "../install.sh")
			cmd.Env = append(os.Environ(), "CAIRN_VERSION="+version, "CAIRN_DOWNLOAD_BASE="+srv.URL+"/"+version,
				"CAIRN_INSTALL="+dest, "HOME="+e.T.TempDir())
			out, err := cmd.CombinedOutput()
			return string(out), err
		}
		installed := func() string {
			out, err := exec.Command(filepath.Join(dest, "cairn"), "version").Output()
			e.Must(err)
			return strings.TrimSpace(string(out))
		}

		fakeRelease(e, filepath.Join(root, "v0.0.1-test"), "v0.0.1-test")
		if out, err := install("v0.0.1-test"); err != nil {
			e.T.Fatalf("install: %v\n%s", err, out)
		}
		Equal(e, "installed version", installed(), "v0.0.1-test")

		// an upgrade replaces it
		fakeRelease(e, filepath.Join(root, "v0.0.2-test"), "v0.0.2-test")
		if out, err := install("v0.0.2-test"); err != nil {
			e.T.Fatalf("upgrade: %v\n%s", err, out)
		}
		Equal(e, "upgraded version", installed(), "v0.0.2-test")

		// a download that does not match SHA256SUMS is refused, and the
		// cairn already there stays
		bad := filepath.Join(root, "v0.0.3-test")
		fakeRelease(e, bad, "v0.0.3-test")
		name := fmt.Sprintf("cairn_v0.0.3-test_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
		f, err := os.OpenFile(filepath.Join(bad, name), os.O_APPEND|os.O_WRONLY, 0)
		e.Must(err)
		_, err = f.Write([]byte("tampered"))
		e.Must(err)
		e.Must(f.Close())
		out, err := install("v0.0.3-test")
		if err == nil || !strings.Contains(out, "checksum mismatch") {
			e.T.Fatalf("tampered download: %v\n%s", err, out)
		}
		Equal(e, "version after a refused install", installed(), "v0.0.2-test")

		// a version with no build for this machine says so
		out, err = install("v9.9.9-missing")
		if err == nil || !strings.Contains(out, "could not download") {
			e.T.Fatalf("missing release: %v\n%s", err, out)
		}
	})
}
