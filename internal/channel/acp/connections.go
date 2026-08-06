package acp

import (
	"crypto/rand"
	"errors"
	"sync"
)

const (
	// A caller holding the token is trusted, so these are not a quota: they keep a
	// client that reconnects without ever sending DELETE from growing momo's
	// memory without bound.
	defaultMaxConnections     = 128
	defaultMaxSessionsPerConn = 64

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

// connections is this endpoint's table of live ACP connections: the ids
// initialize issued, and for each the sessions it hosts and the streams momo
// answers on. It is transport state, not a momo-wide concept — a channel that
// has no connections to track needs nothing like it. The table exists in memory
// only: a restart loses it, and the client has to initialize again.
type connections struct {
	mu   sync.Mutex
	byID map[string]*conn
	// max is how many connections this endpoint holds at once, and maxSessions how
	// many sessions each of them hosts.
	max         int
	maxSessions int
}

func newConnections(max, maxSessions int) *connections {
	return &connections{byID: map[string]*conn{}, max: max, maxSessions: maxSessions}
}

func (cs *connections) create() (string, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.byID) >= cs.max {
		cs.dropAbandoned()
	}
	if len(cs.byID) >= cs.max {
		return "", errTooManyConns
	}
	id := rand.Text()
	cs.byID[id] = &conn{sessions: map[string]bool{}, streams: map[string]*stream{}, maxSessions: cs.maxSessions}
	return id, nil
}

// dropAbandoned frees the connections nobody is listening to, so a client that
// went away without sending DELETE does not hold a slot until momo restarts. A
// client holds a stream for as long as it means to hear from momo; one caught
// between streams is told its connection is unknown and initializes again.
func (cs *connections) dropAbandoned() {
	for id, c := range cs.byID {
		if c.abandoned() {
			delete(cs.byID, id)
			c.close()
		}
	}
}

func (cs *connections) lookup(id string) *conn {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.byID[id]
}

func (cs *connections) remove(id string) *conn {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c := cs.byID[id]
	delete(cs.byID, id)
	return c
}

// conn is one initialized connection: the sessions it hosts and the streams
// momo answers on, keyed by scope. Once closed it accepts nothing more, because
// a request that resolved it a moment earlier still holds it.
type conn struct {
	mu          sync.Mutex
	closed      bool
	sessions    map[string]bool
	maxSessions int
	streams     map[string]*stream
}

func (c *conn) newSession() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", errConnClosed
	}
	if len(c.sessions) >= c.maxSessions {
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
