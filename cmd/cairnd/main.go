// cairnd is the Cairn engine: it runs workspaces and serves the REST API.
//
//	cairnd --data /var/lib/cairn                     # gVisor runtime (Linux)
//	cairnd --data ./data --runtime host              # development only: no isolation
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AQLD/cairn/internal/api"
	"github.com/AQLD/cairn/internal/engine"
	"github.com/AQLD/cairn/internal/runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cairnd:", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:7420", "address for the REST API")
	data := flag.String("data", "/var/lib/cairn", "data folder: state, workspaces and saves")
	rt := flag.String("runtime", "runsc", "workspace runtime: runsc (gVisor) or host (development only, no isolation)")
	runscPath := flag.String("runsc", "runsc", "path to the runsc binary")
	initPath := flag.String("init", "", "path to cairn-init, process 1 of every runsc workspace (default: next to cairnd, then PATH)")
	token := flag.String("token", os.Getenv("CAIRN_TOKEN"), "API token required on every request (env CAIRN_TOKEN)")
	retention := flag.Duration("retention", engine.DefaultRetention, "how long deleted saves can be undeleted before they are purged")
	scrubEvery := flag.Duration("scrub-interval", 6*time.Hour, "how often to re-verify every save's checksums (0 disables)")
	defaultTemplate := flag.String("default-template", "", "template for workspaces created without one")
	idlePause := flag.Duration("idle-pause", 0, "pause workspaces unused this long: full save, memory freed, woken on next use (0 disables; needs runsc)")
	pauseOnShutdown := flag.Bool("pause-on-shutdown", true, "pause every running workspace on SIGTERM so its processes survive a restart or a moved pod (needs runsc)")
	shutdownTimeout := flag.Duration("shutdown-timeout", 2*time.Minute, "how long shutdown may take (pausing workspaces included)")
	var imports templateImports
	flag.Var(&imports, "import-template", "NAME=ROOTFS.tar to import at startup if missing or changed (repeatable)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	var r runtime.Runtime
	switch *rt {
	case "runsc":
		ip, err := findInit(*initPath)
		if err != nil {
			return err
		}
		rs := runtime.NewRunsc(*runscPath, filepath.Join(*data, "runsc"), ip)
		rs.Log = log
		r = rs
	case "host":
		log.Warn("using the host runtime: commands run directly on this machine with NO isolation; development only")
		r = runtime.NewHost()
	default:
		return fmt.Errorf("unknown runtime %q (want runsc or host)", *rt)
	}

	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" && *token == "" {
		return errors.New("refusing to listen beyond localhost without --token: the API runs commands")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng, err := engine.Open(ctx, engine.Config{DataDir: *data, Runtime: r, Logger: log, Retention: *retention, DefaultTemplate: *defaultTemplate})
	if err != nil {
		return err
	}
	defer eng.Close()

	for _, imp := range imports {
		if err := importTemplate(ctx, eng, imp.name, imp.path); err != nil {
			return fmt.Errorf("--import-template %s: %w", imp.name, err)
		}
		log.Info("template ready", "template", imp.name)
	}

	// Re-verify saves in the background, so damage is found before a restore needs them.
	if *scrubEvery > 0 {
		go func() {
			t := time.NewTicker(*scrubEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					checked, bad, err := eng.Scrub(ctx)
					if err != nil {
						log.Error("scrub failed", "err", err)
					} else {
						log.Info("scrub done", "checked", checked, "not_ready", bad)
					}
				}
			}
		}()
	}

	// Purge deleted saves whose retention window has ended (also done at startup).
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := eng.PurgeExpired(ctx); err != nil {
					log.Error("purge failed", "err", err)
				}
			}
		}
	}()

	// Pause workspaces nobody is using, freeing their memory.
	if *idlePause > 0 {
		go func() {
			t := time.NewTicker(min(*idlePause/4, time.Minute))
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if n, err := eng.PauseIdle(ctx, *idlePause); err != nil {
						log.Error("pausing idle workspaces failed", "err", err)
					} else if n > 0 {
						log.Info("paused idle workspaces", "count", n)
					}
				}
			}
		}()
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.New(eng, log, *token),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("cairnd listening", "addr", *listen, "runtime", r.Name(), "data", *data)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Stop taking requests and, at the same time, pause the workspaces (which
	// ends running commands, so their requests finish too).
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Shutdown(shutdown) }()
	if *pauseOnShutdown {
		start := time.Now()
		paused, failed := eng.PauseAll(shutdown)
		log.Info("paused workspaces for shutdown", "paused", paused, "failed", failed, "took", time.Since(start).Round(time.Millisecond))
	}
	return <-srvDone
}

type templateImport struct{ name, path string }

// templateImports collects repeated --import-template NAME=PATH flags.
type templateImports []templateImport

func (t *templateImports) String() string { return fmt.Sprint(*t) }

func (t *templateImports) Set(v string) error {
	name, path, ok := strings.Cut(v, "=")
	if !ok || name == "" || path == "" {
		return errors.New("want NAME=ROOTFS.tar")
	}
	*t = append(*t, templateImport{name, path})
	return nil
}

// importTemplate imports a rootfs tar unless the engine already has a template
// with that name and the same content, so restarts are cheap.
func importTemplate(ctx context.Context, eng *engine.Engine, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if t, err := eng.GetTemplate(ctx, name); err == nil && t.SHA256 == hex.EncodeToString(h.Sum(nil)) {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = eng.ImportTemplate(ctx, name, f)
	return err
}

// findInit locates cairn-init: the flag, then next to this binary, then PATH.
func findInit(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), "cairn-init")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("cairn-init"); err == nil {
		return p, nil
	}
	return "", errors.New("cairn-init not found: install it next to cairnd or pass --init (build it with `go build ./cmd/cairn-init`)")
}
