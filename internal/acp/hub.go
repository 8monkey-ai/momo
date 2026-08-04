package acp

import (
	"crypto/rand"
	"sync"
)

// maxConnections bounds the connection table. Nothing evicts a connection a
// client abandoned without a DELETE, and this transport has no keepalive to
// notice one, so the table needs a ceiling of its own.
const maxConnections = 256

// maxSessions bounds one connection's sessions, for the same reason: they are
// released when the connection is, and nothing else releases them.
const maxSessions = 64

// hub holds the connections momo has issued an id for.
type hub struct {
	mu    sync.Mutex
	conns map[string]*connection
}

func newHub() *hub {
	return &hub{conns: map[string]*connection{}}
}

// open registers a new connection, reporting false when the table is full.
func (h *hub) open() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.conns) >= maxConnections {
		return "", false
	}
	id := rand.Text()
	h.conns[id] = &connection{sessions: map[string]*stream{}}
	return id, true
}

func (h *hub) lookup(id string) (*connection, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, known := h.conns[id]
	return c, known
}

// remove takes the connection out of the table and closes its streams, which
// releases its sessions with it.
func (h *hub) remove(id string) bool {
	h.mu.Lock()
	c, known := h.conns[id]
	delete(h.conns, id)
	h.mu.Unlock()
	if !known {
		return false
	}
	c.close()
	return true
}

// connection is one initialized client connection and the sessions it hosts. A
// session with no stream open is present in sessions with a nil value.
type connection struct {
	mu       sync.Mutex
	stream   *stream
	sessions map[string]*stream
	gone     bool
}

// newSession reports false when the connection is holding as many sessions as
// it may.
func (c *connection) newSession() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sessions) >= maxSessions {
		return "", false
	}
	id := rand.Text()
	c.sessions[id] = nil
	return id, true
}

func (c *connection) hasSession(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, known := c.sessions[id]
	return known
}

// attach makes s the stream for a session, or for the connection itself when
// sessionID is empty, closing the stream it replaces. A client that reconnects
// before momo noticed the old stream was dead gets the new one.
func (c *connection) attach(sessionID string, s *stream) {
	c.mu.Lock()
	// A connection terminated while this stream was being opened keeps nothing:
	// the stream is closed instead of attached, so DELETE really does end every
	// stream of the connection it removed.
	if c.gone {
		c.mu.Unlock()
		s.close()
		return
	}
	replaced := c.set(sessionID, s)
	c.mu.Unlock()
	if replaced != nil {
		replaced.close()
	}
}

// send delivers b on the stream for a session, or on the connection's own
// stream when sessionID is empty. A scope with no stream open drops the
// message: this transport version has no resumption.
func (c *connection) send(sessionID string, b []byte) {
	c.mu.Lock()
	s := c.get(sessionID)
	c.mu.Unlock()
	if s != nil {
		s.send(b)
	}
}

func (c *connection) close() {
	c.mu.Lock()
	streams := make([]*stream, 0, len(c.sessions)+1)
	streams = append(streams, c.stream)
	for _, s := range c.sessions {
		streams = append(streams, s)
	}
	c.stream = nil
	clear(c.sessions)
	c.gone = true
	c.mu.Unlock()
	for _, s := range streams {
		if s != nil {
			s.close()
		}
	}
}

func (c *connection) get(sessionID string) *stream {
	if sessionID == "" {
		return c.stream
	}
	return c.sessions[sessionID]
}

func (c *connection) set(sessionID string, s *stream) *stream {
	previous := c.get(sessionID)
	if sessionID == "" {
		c.stream = s
	} else {
		c.sessions[sessionID] = s
	}
	return previous
}
