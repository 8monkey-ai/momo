// Command momo runs the momo server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/config"
	"github.com/8monkey-ai/momo/internal/core"

	_ "github.com/8monkey-ai/momo/internal/channel/acp"
	_ "github.com/8monkey-ai/momo/internal/channel/respondio"
)

const healthPath = "/healthz"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("momo stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	path := flag.String("config", "/etc/momo/momo.yaml", "path to the configuration file")
	flag.Parse()

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The channels' lifetime is this context: the signal that starts the shutdown
	// is what makes them let go of what they hold.
	instances, err := channel.Build(ctx, cfg.Channels, core.LogHandler{Log: log})
	if err != nil {
		return err
	}
	mux, err := buildMux(instances, log)
	if err != nil {
		return err
	}

	return serve(ctx, log, &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Timeouts.ReadHeader,
		ReadTimeout:       cfg.Timeouts.Read,
		IdleTimeout:       cfg.Timeouts.Idle,
	}, cfg.Timeouts.Shutdown)
}

func buildMux(instances []channel.Instance, log *slog.Logger) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		// Nothing can be done if the monitor hung up mid-response.
		_, _ = fmt.Fprintln(w, "ok")
	})
	served := map[string]bool{healthPath: true}
	for _, in := range instances {
		paths := make([]string, 0, len(in.Channel.Routes()))
		for _, route := range in.Channel.Routes() {
			// http.ServeMux panics on a duplicate path; a mistake in the
			// configuration file has to be reported, not crash the process.
			if served[route.Path] {
				return nil, fmt.Errorf("channel %q: path %q is already served", in.Name, route.Path)
			}
			served[route.Path] = true
			if err := handle(mux, route); err != nil {
				return nil, fmt.Errorf("channel %q: %w", in.Name, err)
			}
			paths = append(paths, route.Path)
		}
		log.Info("channel ready", "channel", in.Name, "paths", paths)
	}
	return mux, nil
}

// handle registers one route, turning the panic http.ServeMux raises on a path
// it cannot parse into an error naming that path. Recovering leaves the pattern
// grammar to net/http instead of restating it here.
func handle(mux *http.ServeMux, route channel.Route) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("path %q cannot be served: %v", route.Path, r)
		}
	}()
	mux.Handle(route.Path, route.Handler)
	return nil
}

func serve(ctx context.Context, log *slog.Logger, srv *http.Server, shutdownTimeout time.Duration) error {
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	log.Info("🐒 momo listening", "address", srv.Addr, "health", healthPath)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down, waiting for in-flight requests")
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdown)
}
