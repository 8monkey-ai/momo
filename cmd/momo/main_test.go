package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/8monkey-ai/momo/internal/channel"
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
