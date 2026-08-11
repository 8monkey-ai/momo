// Command momo runs the momo server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/net/netutil"

	"github.com/8monkey-ai/momo/internal/agent"
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
	l, err := listen(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, log, cfg, l)
}

// listen enforces the configured maximum on accept, before a request exists: an
// SSE stream holding a connection for hours is what the cap accounts for, and a
// request already accepted is never refused for it.
func listen(cfg *config.Config) (net.Listener, error) {
	l, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	return netutil.LimitListener(l, cfg.MaxConnections), nil
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

func serve(ctx context.Context, log *slog.Logger, cfg *config.Config, l net.Listener) error {
	lifetime, release := context.WithCancel(context.Background())
	defer release()

	a, err := agent.New(cfg.Agent, log)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	instances, err := channel.Build(lifetime, cfg.Channels, core.AgentHandler{Agent: a, Log: log})
	if err != nil {
		return err
	}
	mux, err := buildMux(instances, log)
	if err != nil {
		return err
	}
	// No WriteTimeout: any value cuts a stream a channel is still writing.
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(l) }()
	log.Info("🐒 momo listening", "address", l.Addr().String(), "health", healthPath, "max_connections", cfg.MaxConnections)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down, waiting for in-flight requests")
	// Channels release their streams first: Shutdown waits for handlers to
	// return, and a stream never returns on its own.
	release()
	shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdown)
}
