package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/config"
)

type fixed struct {
	routes []channel.Route
}

func (f fixed) Routes() []channel.Route { return f.routes }

func instance(name string, paths ...string) channel.Instance {
	routes := make([]channel.Route, 0, len(paths))
	for _, p := range paths {
		routes = append(routes, channel.Route{Path: p, Handler: marker(p)})
	}
	return channel.Instance{Name: name, Channel: fixed{routes: routes}}
}

func marker(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func body(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", path, w.Code, http.StatusOK)
	}
	return strings.TrimSpace(w.Body.String())
}

func TestMuxServesHealthAndChannelRoutes(t *testing.T) {
	mux, err := buildMux([]channel.Instance{instance("stub", "/stub/in")}, discard())
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	if got := body(t, mux, healthPath); got != "ok" {
		t.Errorf("%s answered %q, want \"ok\"", healthPath, got)
	}
	if got := body(t, mux, "/stub/in"); got != "/stub/in" {
		t.Errorf("/stub/in answered %q, want the channel's own handler", got)
	}
}

func TestMuxReportsAnUnusablePath(t *testing.T) {
	// http.ServeMux panics on these, so a typo in the configuration file would
	// take the process down instead of being reported.
	for _, path := range []string{"", "/respondio/{", "/respond io"} {
		t.Run(path, func(t *testing.T) {
			if _, err := buildMux([]channel.Instance{instance("stub", path)}, discard()); err == nil {
				t.Fatalf("buildMux(%q) succeeded, want an error naming the path", path)
			}
		})
	}
}

func TestMuxReportsAPathServedTwice(t *testing.T) {
	for _, tc := range []struct {
		name      string
		instances []channel.Instance
	}{
		{name: "same path twice in one channel", instances: []channel.Instance{instance("stub", "/in", "/in")}},
		{name: "path shared by two channels", instances: []channel.Instance{instance("a", "/in"), instance("b", "/in")}},
		{name: "path taken by health", instances: []channel.Instance{instance("a", healthPath)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A duplicate would panic inside http.ServeMux.
			if _, err := buildMux(tc.instances, discard()); err == nil {
				t.Fatal("buildMux succeeded, want an error naming the duplicate path")
			}
		})
	}
}

func loadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "momo.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// TestShutdownDoesNotWaitForAnOpenStream drives shutdown the way run does: the
// process cancels the context, and the channels are told through their lifetime.
func TestShutdownDoesNotWaitForAnOpenStream(t *testing.T) {
	cfg := loadConfig(t, "listen: \"127.0.0.1:0\"\nshutdown_timeout: 30s\nchannels:\n  acp:\n    token: secret\n")
	l, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- serve(ctx, discard(), cfg, l) }()

	endpoint := "http://" + l.Addr().String() + "/v1/acp"
	connID := initialize(t, endpoint)
	stream, err := open(endpoint, connID)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()

	stop()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for the open stream instead of closing it")
	}
}

func initialize(t *testing.T, endpoint string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`
	r, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	return resp.Header.Get("Acp-Connection-Id")
}

func open(endpoint, connID string) (*http.Response, error) {
	r, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Acp-Connection-Id", connID)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stream = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	return resp, nil
}

func TestListenerStopsAcceptingPastTheConfiguredMaximum(t *testing.T) {
	cfg := loadConfig(t, "listen: \"127.0.0.1:0\"\nmax_connections: 1\n")
	l, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: marker("ok"), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()

	held, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := &http.Client{
		Timeout:   300 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	if _, err := client.Get("http://" + l.Addr().String()); err == nil {
		t.Fatal("a second connection was served while the maximum was reached")
	}

	_ = held.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + l.Addr().String())
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the freed slot was never served: %v", err)
		}
	}
}
