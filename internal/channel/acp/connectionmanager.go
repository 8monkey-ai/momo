package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// stream is one SSE listener: the GET handler that opened it reads frames until
// the client goes away or the connection is released.
type stream struct {
	frames chan []byte
	closed chan struct{}
	once   sync.Once
}

func newStream() *stream {
	return &stream{frames: make(chan []byte, 32), closed: make(chan struct{})}
}

// send never blocks: this transport has no resumption, so a frame a listener
// cannot keep up with is lost exactly as one emitted while nobody listened.
func (s *stream) send(frame []byte) {
	select {
	case s.frames <- frame:
	default:
	}
}

func (s *stream) close() { s.once.Do(func() { close(s.closed) }) }

type connection struct {
	// sessions maps a session id to its stream, nil while nothing listens to it.
	sessions map[string]*stream
	stream   *stream
	// unlistenedSince is zero while any stream is attached, and otherwise the
	// moment the last one detached.
	unlistenedSince time.Time
}

// connectionManager holds the live connections and their sessions. Sessions
// exist in memory only: they are released with their connection and do not
// survive a restart.
type connectionManager struct {
	grace time.Duration
	now   func() time.Time

	mu    sync.Mutex
	conns map[string]*connection
}

func newConnectionManager(grace time.Duration, now func() time.Time) *connectionManager {
	return &connectionManager{grace: grace, now: now, conns: map[string]*connection{}}
}

// run reclaims connections nothing listens to and releases everything when the
// lifetime momo handed the channel ends.
func (m *connectionManager) run(lifetime context.Context) {
	ticker := time.NewTicker(m.grace)
	defer ticker.Stop()
	for {
		select {
		case <-lifetime.Done():
			m.releaseAll()
			return
		case <-ticker.C:
			m.sweep()
		}
	}
}

func (m *connectionManager) newConnection() string {
	id := newID()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[id] = &connection{sessions: map[string]*stream{}, unlistenedSince: m.now()}
	return id
}

func (m *connectionManager) newSession(connID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, known := m.conns[connID]
	if !known {
		return "", false
	}
	// ponytail: momo's session id is the only one in play today. Once the agent
	// harness lands the upstream agent issues one of its own and the two have to
	// be mapped, because the id momo hands the client must stay stable across
	// whatever the upstream does.
	id := newID()
	c.sessions[id] = nil
	return id, true
}

func (m *connectionManager) exists(connID, sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, known := m.conns[connID]
	if !known {
		return false
	}
	if sessionID == "" {
		return true
	}
	_, known = c.sessions[sessionID]
	return known
}

// listen attaches a stream to a connection or to one of its sessions, replacing
// any stream already attached there.
func (m *connectionManager) listen(connID, sessionID string) (*stream, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, known := m.conns[connID]
	if !known {
		return nil, false
	}
	s := newStream()
	if sessionID == "" {
		closeStream(c.stream)
		c.stream = s
	} else {
		attached, known := c.sessions[sessionID]
		if !known {
			return nil, false
		}
		closeStream(attached)
		c.sessions[sessionID] = s
	}
	c.unlistenedSince = time.Time{}
	return s, true
}

func closeStream(s *stream) {
	if s != nil {
		s.close()
	}
}

// stopListening detaches a stream that the GET handler is leaving, unless the
// client has already reconnected and put another one in its place.
func (m *connectionManager) stopListening(connID, sessionID string, s *stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, known := m.conns[connID]
	if !known {
		return
	}
	if sessionID == "" {
		if c.stream == s {
			c.stream = nil
		}
	} else if c.sessions[sessionID] == s {
		c.sessions[sessionID] = nil
	}
	if !listening(c) {
		c.unlistenedSince = m.now()
	}
}

func listening(c *connection) bool {
	if c.stream != nil {
		return true
	}
	for _, s := range c.sessions {
		if s != nil {
			return true
		}
	}
	return false
}

// send delivers a frame on the connection-scoped stream when sessionID is empty
// and on that session's stream otherwise.
func (m *connectionManager) send(connID, sessionID string, frame []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, known := m.conns[connID]
	if !known {
		return
	}
	s := c.stream
	if sessionID != "" {
		attached, known := c.sessions[sessionID]
		if !known {
			return
		}
		s = attached
	}
	if s != nil {
		s.send(frame)
	}
}

// release drops a connection, its sessions and its streams.
func (m *connectionManager) release(connID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, known := m.conns[connID]
	if !known {
		return false
	}
	delete(m.conns, connID)
	closeStreams(c)
	return true
}

func (m *connectionManager) releaseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.conns {
		delete(m.conns, id)
		closeStreams(c)
	}
}

// sweep drops the connections nothing has listened to for the grace period. The
// grace exists because a client is legitimately unlistened-to between
// initialize and its first stream.
func (m *connectionManager) sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.conns {
		if c.unlistenedSince.IsZero() || m.now().Sub(c.unlistenedSince) < m.grace {
			continue
		}
		delete(m.conns, id)
		closeStreams(c)
	}
}

func closeStreams(c *connection) {
	closeStream(c.stream)
	for _, s := range c.sessions {
		closeStream(s)
	}
}

func newID() string {
	b := make([]byte, 16)
	// crypto/rand.Read never fails as of Go 1.24.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
