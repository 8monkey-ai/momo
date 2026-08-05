package acp

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// A stream that momo cannot write to fast enough loses messages rather than
// blocking the request that produced them; this transport has no resumption, so
// a message the client missed is gone either way.
const streamBuffer = 32

// A client that stops reading fills the socket buffer and leaves momo blocked
// mid-write, where neither close nor shutdown can reach it. Every write is
// bounded so that client cannot hold a stream, or momo's shutdown, open.
const streamWriteTimeout = 5 * time.Second

// stream is one SSE response momo writes server-to-client messages on.
type stream struct {
	msgs   chan []byte
	closed chan struct{}
	once   sync.Once
}

func newStream() *stream {
	return &stream{msgs: make(chan []byte, streamBuffer), closed: make(chan struct{})}
}

func (s *stream) send(msg []byte) {
	select {
	case s.msgs <- msg:
	default:
	}
}

func (s *stream) close() { s.once.Do(func() { close(s.closed) }) }

// serve writes messages until the stream is closed, the client goes away, or
// momo's shutdown ends the channel's lifetime.
func (s *stream) serve(ctx context.Context, life <-chan struct{}, w http.ResponseWriter) {
	w.Header().Set("Content-Type", sseMediaType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// Flushing an empty response tells the client the stream is open before the
	// first message arrives.
	if err := rc.Flush(); err != nil {
		return
	}
	for {
		select {
		case msg := <-s.msgs:
			// A server that cannot set the deadline keeps the unbounded behavior; that
			// is no worse than not asking.
			_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-s.closed:
			return
		case <-ctx.Done():
			return
		case <-life:
			return
		}
	}
}
