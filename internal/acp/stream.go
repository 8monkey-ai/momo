package acp

import (
	"fmt"
	"net/http"
	"sync"
)

// streamBuffer is how many server-to-client messages a stream holds for a
// client that is not reading.
const streamBuffer = 32

// stream is one SSE response momo writes server-to-client messages to. A client
// opens one per scope: one for the connection, one for each session.
type stream struct {
	msgs   chan []byte
	closed chan struct{}
	once   sync.Once
}

func newStream() *stream {
	return &stream{msgs: make(chan []byte, streamBuffer), closed: make(chan struct{})}
}

// send never blocks the caller: a stream nobody is reading drops the message.
// ponytail: v1 of this transport replays nothing, so a dropped message is the
// specified behaviour rather than a defect; give the stream real backpressure
// once an agent's replies ride on it.
func (s *stream) send(b []byte) {
	select {
	case s.msgs <- b:
	default:
	}
}

func (s *stream) close() {
	s.once.Do(func() { close(s.closed) })
}

// writeTo copies messages to w until the client hangs up, the stream is
// replaced by a newer one for the same scope, or momo shuts down.
func (s *stream) writeTo(w http.ResponseWriter, f http.Flusher, hangup, shutdown <-chan struct{}) {
	f.Flush()
	for {
		select {
		case b := <-s.msgs:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			f.Flush()
		case <-s.closed:
			return
		case <-hangup:
			return
		case <-shutdown:
			return
		}
	}
}
