package acp

import (
	"crypto/rand"
	"errors"
	"sync"
	"time"
)

const (
	// abandonAfter is how long a connection with nothing listening to it is kept
	// before the sweep drops it. The grace covers the gap between initialize and
	// the first stream, so a client that is still on its way is not dropped.
	abandonAfter = 30 * time.Second

	// connectionScope is the key of the connection-scoped stream, the one scope
	// that is not a session.
	connectionScope = ""
)

var (
	errConnClosed = errors.New("connection closed")
	errScopeTaken = errors.New("a stream is already open for this scope")
)

// connectionManager holds this endpoint's live ACP connections: the ids
// initialize issued, and for each the sessions it hosts and the streams momo
// answers on. It is transport state, not a momo-wide concept — a channel that
// has no connections to track needs nothing like it. It holds them in memory
// only: a restart loses them, and the client has to initialize again.
type connectionManager struct {
	mu   sync.Mutex
	byID map[string]*conn
}

func newConnectionManager() *connectionManager {
	return &connectionManager{byID: map[string]*conn{}}
}

func (cm *connectionManager) create() string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.dropAbandoned()
	id := rand.Text()
	cm.byID[id] = &conn{idleSince: time.Now(), sessions: map[string]bool{}, streams: map[string]*stream{}}
	return id
}

// dropAbandoned frees the connections nobody has been listening to for
// abandonAfter, so a client that went away without sending DELETE does not hold
// its record until momo restarts. A client holds a stream for as long as it means
// to hear from momo; one dropped between streams is told its connection is
// unknown and initializes again.
func (cm *connectionManager) dropAbandoned() {
	for id, c := range cm.byID {
		if c.abandoned() {
			delete(cm.byID, id)
			c.close()
		}
	}
}

func (cm *connectionManager) lookup(id string) *conn {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.byID[id]
}

func (cm *connectionManager) remove(id string) *conn {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	c := cm.byID[id]
	delete(cm.byID, id)
	return c
}

// conn is one initialized connection: the sessions it hosts and the streams
// momo answers on, keyed by scope. Once closed it accepts nothing more, because
// a request that resolved it a moment earlier still holds it.
type conn struct {
	mu        sync.Mutex
	closed    bool
	idleSince time.Time
	sessions  map[string]bool
	streams   map[string]*stream
}

func (c *conn) newSession() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", errConnClosed
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

// abandoned reports whether nothing has been listening to this connection for
// abandonAfter.
func (c *conn) abandoned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.streams) == 0 && time.Since(c.idleSince) > abandonAfter
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
		c.idleSince = time.Now()
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
