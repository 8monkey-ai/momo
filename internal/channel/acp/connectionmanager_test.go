package acp

import (
	"context"
	"testing"
	"time"
)

func TestSweepDropsOnlyAbandonedConnections(t *testing.T) {
	now := time.Now()
	m := newConnectionManager(5*time.Minute, func() time.Time { return now })

	listened := m.newConnection()
	if _, known := m.listen(listened, ""); !known {
		t.Fatal("listen on a fresh connection failed")
	}
	abandoned := m.newConnection()
	now = now.Add(6 * time.Minute)
	// This one is still inside its grace: a client is legitimately unlistened-to
	// between initialize and its first stream.
	waiting := m.newConnection()

	m.sweep()

	if m.exists(abandoned, "") {
		t.Error("the abandoned connection survived the sweep")
	}
	if !m.exists(listened, "") {
		t.Error("a connection with a stream attached was dropped")
	}
	if !m.exists(waiting, "") {
		t.Error("a connection still inside its grace was dropped")
	}
}

func TestConnectionIsAbandonedAgainWhenItsLastStreamDetaches(t *testing.T) {
	now := time.Now()
	m := newConnectionManager(time.Minute, func() time.Time { return now })

	connID := m.newConnection()
	s, _ := m.listen(connID, "")
	now = now.Add(2 * time.Minute)
	m.sweep()
	if !m.exists(connID, "") {
		t.Fatal("a listened-to connection was dropped")
	}

	m.stopListening(connID, "", s)
	m.sweep()
	if !m.exists(connID, "") {
		t.Fatal("the connection was dropped as soon as the stream detached, before its grace")
	}
	now = now.Add(2 * time.Minute)
	m.sweep()
	if m.exists(connID, "") {
		t.Fatal("the connection survived a grace period with nothing listening")
	}
}

func TestLifetimeEndingReleasesEverything(t *testing.T) {
	m := newConnectionManager(time.Minute, time.Now)
	connID := m.newConnection()
	sessionID, _ := m.newSession(connID)
	sessionStream, _ := m.listen(connID, sessionID)

	lifetime, release := context.WithCancel(context.Background())
	go m.run(lifetime)
	release()

	select {
	case <-sessionStream.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream stayed open after the channel's lifetime ended")
	}
}
