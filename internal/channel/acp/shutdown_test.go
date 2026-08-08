package acp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readTimeout stands in for the read timeout momo's server runs with. A stream
// has to outlive it, and momo's shutdown must not wait for the stream.
const readTimeout = 200 * time.Millisecond

// A client that connects once momo has begun shutting down must not be given a
// connection it will never be answered on.
func TestInitializeIsRefusedOnceMomoIsShuttingDown(t *testing.T) {
	life, stopping := context.WithCancel(context.Background())
	c, calls := newChannel(t, life)
	srv := httptest.NewServer(c.Routes()[0].Handler)
	t.Cleanup(srv.Close)
	p := peerAt(t, srv.URL, c, calls)

	stopping()

	resp := p.post(initializeBody, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("initialize = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "momo is shutting down") {
		t.Errorf("initialize said %q, want it to say momo is shutting down", body)
	}
}

func TestShutdownClosesStreamsInsteadOfWaitingForThem(t *testing.T) {
	life, stopping := context.WithCancel(context.Background())
	c, calls := newChannel(t, life)
	srv := &http.Server{
		Handler:           c.Routes()[0].Handler,
		ReadHeaderTimeout: readTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       time.Second,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()

	p := peerAt(t, "http://"+ln.Addr().String(), c, calls)
	id := p.initialize()
	connStream := p.stream(map[string]string{connectionHeader: id})

	time.Sleep(2 * readTimeout)
	// The answer still lands, so the read timeout did not cut the stream.
	p.newSession(id, connStream)

	stopping()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("Shutdown waited %v, want it not to wait for the open stream", waited)
	}
	select {
	case <-connStream.ended:
	case <-time.After(2 * time.Second):
		t.Error("the stream was still open after shutdown")
	}
}
