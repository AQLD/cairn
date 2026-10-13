// cairnd is the Cairn engine: it runs workspaces and serves the REST API.
//
//	cairnd --data /var/lib/cairn                     # gVisor runtime (Linux)
//	cairnd --data ./data --runtime host              # development only: no isolation
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	token := flag.String("token", os.Getenv("CAIRN_TOKEN"), "API token required on every request (env CAIRN_TOKEN)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	var r runtime.Runtime
	switch *rt {
	case "runsc":
		r = runtime.NewRunsc(*runscPath)
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

	eng, err := engine.Open(ctx, engine.Config{DataDir: *data, Runtime: r, Logger: log})
	if err != nil {
		return err
	}
	defer eng.Close()

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
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
