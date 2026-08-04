package acp

import (
	"crypto/rand"
	"errors"
	"maps"
	"slices"
	"sync"
)

const (
	// A caller holding the token is trusted, so these are not a quota: they keep a
	// client that reconnects without ever sending DELETE from growing momo's
	// memory without bound.
	maxConnections     = 128
	maxSessionsPerConn = 64

	// connectionScope is the key of the connection-scoped stream, the one scope
	// that is not a session.
	connectionScope = ""
)

var (
	errStopping        = errors.New("momo is shutting down")
	errTooManyConns    = errors.New("too many open connections")
	errTooManySessions = errors.New("too many sessions on this connection")
	errConnClosed      = errors.New("connection closed")
	errScopeTaken      = errors.New("a stream is already open for this scope")
)

// registry holds the live connections. They exist in memory only: a restart
// loses them, and the client has to initialize again.
type registry struct {
	mu       sync.Mutex
	conns    map[string]*conn
	stopping bool
}

func newRegistry() *registry { return &registry{conns: map[string]*conn{}} }

func (g *registry) create() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopping {
		return "", errStopping
	}
	if len(g.conns) >= maxConnections {
		g.dropAbandoned()
	}
	if len(g.conns) >= maxConnections {
		return "", errTooManyConns
	}
	id := rand.Text()
	g.conns[id] = &conn{sessions: map[string]bool{}, streams: map[string]*stream{}}
	return id, nil
}

// dropAbandoned frees the connections nobody is listening to, so a client that
// went away without sending DELETE does not hold a slot until momo restarts. A
// client holds a stream for as long as it means to hear from momo; one caught
// between streams is told its connection is unknown and initializes again.
func (g *registry) dropAbandoned() {
	for id, c := range g.conns {
		if c.abandoned() {
			delete(g.conns, id)
			c.close()
		}
	}
}

func (g *registry) lookup(id string) *conn {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conns[id]
}

func (g *registry) remove(id string) *conn {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.conns[id]
	delete(g.conns, id)
	return c
}

// stop drops every connection and closes its streams. A request arriving after
// it finds no connection, so no stream can be opened again.
func (g *registry) stop() {
	g.mu.Lock()
	g.stopping = true
	conns := slices.Collect(maps.Values(g.conns))
	g.conns = map[string]*conn{}
	g.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

// conn is one initialized connection: the sessions it hosts and the streams
// momo answers on, keyed by scope. Once closed it accepts nothing more, because
// a request that resolved it a moment earlier still holds it.
type conn struct {
	mu       sync.Mutex
	closed   bool
	sessions map[string]bool
	streams  map[string]*stream
}

func (c *conn) newSession() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", errConnClosed
	}
	if len(c.sessions) >= maxSessionsPerConn {
		return "", errTooManySessions
	}
	// ponytail: momo issues the session id, and today it is the only session id in
	// play. Once the agent harness lands, the upstream agent will issue one of its
	// own and the two will have to be mapped: the id momo hands the client has to
	// stay stable whatever the upstream does.
	id := rand.Text()
	c.sessions[id] = true
	return id, nil
}

func (c *conn) hasSession(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[id]
}

// abandoned reports whether nothing is listening to this connection.
func (c *conn) abandoned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.streams) == 0
}

// attach claims a scope for s. One writer per scope keeps routing unambiguous,
// so a scope already streaming is refused.
func (c *conn) attach(scope string, s *stream) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errConnClosed
	}
	if c.streams[scope] != nil {
		return errScopeTaken
	}
	c.streams[scope] = s
	return nil
}

func (c *conn) detach(scope string, s *stream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams[scope] == s {
		delete(c.streams, scope)
	}
}

func (c *conn) send(scope string, msg []byte) {
	c.mu.Lock()
	s := c.streams[scope]
	c.mu.Unlock()
	if s != nil {
		s.send(msg)
	}
}

func (c *conn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, s := range c.streams {
		s.close()
	}
	c.streams = map[string]*stream{}
	c.sessions = map[string]bool{}
}
