package acp

import (
	"crypto/rand"
	"sync"
	"time"
)

// streamBuffer is how many server-to-client messages wait on a stream before a
// sender starts dropping them.
const streamBuffer = 16

// stream is one SSE response momo writes server-to-client messages on.
type stream struct {
	messages chan []byte
	// closed is closed when momo drops the stream, so the response returns and
	// a sender stops waiting on a client that is gone.
	closed chan struct{}
}

type connection struct {
	sessions map[string]bool
	// streams is keyed by session id, with the connection-scoped stream under "".
	streams map[string]*stream
	// unlistenedSince is when the connection last lost its final stream, and is
	// zero while one is open.
	unlistenedSince time.Time
}

// connectionManager holds the live connections and the sessions on them.
type connectionManager struct {
	grace time.Duration
	// done is closed when momo is shutting down.
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	conns    map[string]*connection
}

func newConnectionManager(grace time.Duration) *connectionManager {
	m := &connectionManager{grace: grace, done: make(chan struct{}), conns: map[string]*connection{}}
	go m.sweepEvery(grace)
	return m
}

// stop releases every open stream, so nothing momo serves outlives it.
func (m *connectionManager) stop() {
	m.stopOnce.Do(func() { close(m.done) })
}

func (m *connectionManager) open() string {
	id := rand.Text()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[id] = &connection{
		sessions:        map[string]bool{},
		streams:         map[string]*stream{},
		unlistenedSince: time.Now(),
	}
	return id
}

func (m *connectionManager) openSession(connID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[connID]
	if c == nil {
		return "", false
	}
	id := rand.Text()
	c.sessions[id] = true
	return id, true
}

// known reports whether the connection exists and, for a session-scoped
// request, whether that session is one of its own.
func (m *connectionManager) known(connID, sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[connID]
	return c != nil && (sessionID == "" || c.sessions[sessionID])
}

func (m *connectionManager) listen(connID, sessionID string) (*stream, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[connID]
	if c == nil || (sessionID != "" && !c.sessions[sessionID]) {
		return nil, false
	}
	if previous := c.streams[sessionID]; previous != nil {
		close(previous.closed)
	}
	s := &stream{messages: make(chan []byte, streamBuffer), closed: make(chan struct{})}
	c.streams[sessionID] = s
	c.unlistenedSince = time.Time{}
	return s, true
}

func (m *connectionManager) unlisten(connID, sessionID string, s *stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[connID]
	if c == nil || c.streams[sessionID] != s {
		return
	}
	delete(c.streams, sessionID)
	close(s.closed)
	if len(c.streams) == 0 {
		c.unlistenedSince = time.Now()
	}
}

// send delivers a message on the stream the scope names. v1 of the transport has
// no resumption, so a message is lost when nothing is listening, and equally
// when the stream that is listening is not keeping up with what momo sends.
func (m *connectionManager) send(connID, sessionID string, msg []byte) {
	m.mu.Lock()
	c := m.conns[connID]
	var s *stream
	if c != nil {
		s = c.streams[sessionID]
	}
	m.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case s.messages <- msg:
	default:
	}
}

// drop terminates a connection, releasing its sessions and closing its streams.
func (m *connectionManager) drop(connID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.conns[connID]
	if c == nil {
		return false
	}
	delete(m.conns, connID)
	for _, s := range c.streams {
		close(s.closed)
	}
	return true
}

// sweep drops the connections nothing has listened to for the grace period. A
// client is legitimately unlistened-to between initialize and its first stream,
// which is what the grace is for.
func (m *connectionManager) sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.conns {
		if len(c.streams) == 0 && time.Since(c.unlistenedSince) > m.grace {
			delete(m.conns, id)
		}
	}
}

func (m *connectionManager) sweepEvery(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.sweep()
		case <-m.done:
			return
		}
	}
}
