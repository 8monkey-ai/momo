package acp

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
)

// readTimeout stands in for the read timeout momo's server runs with. A stream
// has to outlive it, and momo's shutdown must not wait for the stream.
const readTimeout = 200 * time.Millisecond

func TestShutdownClosesStreamsInsteadOfWaitingForThem(t *testing.T) {
	c, calls := newChannel(t)
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

	channel.Stop([]channel.Instance{{Name: "acp", Channel: c}})
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
