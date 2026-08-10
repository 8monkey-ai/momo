package acp

import (
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	built, err := New(func(v any) error {
		v.(*settings).Token = "secret"
		return nil
	}, capture{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := built.(*acp)
	t.Cleanup(a.conns.stop)

	routes := a.Routes()
	if len(routes) != 1 || routes[0].Path != "/acp/v1" {
		t.Fatalf("routes = %+v, want the one default endpoint", routes)
	}
	if a.conns.grace != 5*time.Minute {
		t.Fatalf("connection grace = %v, want 5m", a.conns.grace)
	}
}

func TestNewRequiresAToken(t *testing.T) {
	if _, err := New(func(any) error { return nil }, capture{}); err == nil {
		t.Fatal("New succeeded without a token, want an error")
	}
}

func TestSendDoesNotBlockOnAFullStream(t *testing.T) {
	m := newConnectionManager(time.Minute)
	t.Cleanup(m.stop)

	connID := m.open()
	s, ok := m.listen(connID, "")
	if !ok {
		t.Fatal("the connection cannot be listened to")
	}
	for range cap(s.messages) {
		m.send(connID, "", []byte("filler"))
	}

	sent := make(chan struct{})
	go func() {
		m.send(connID, "", []byte("one too many"))
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("send is still waiting on a stream whose buffer is full")
	}

	m.unlisten(connID, "", s)
	select {
	case <-s.closed:
	default:
		t.Fatal("unlisten left the stream open after removing it")
	}
}

func TestSweepDropsOnlyAbandonedConnections(t *testing.T) {
	grace := 60 * time.Millisecond
	m := newConnectionManager(grace)
	t.Cleanup(m.stop)

	listening := m.open()
	if _, ok := m.listen(listening, ""); !ok {
		t.Fatal("the connection cannot be listened to")
	}
	abandoned := m.open()
	time.Sleep(2 * grace)
	insideGrace := m.open()

	m.sweep()

	if m.known(abandoned, "") {
		t.Fatal("a connection nothing listened to for the whole grace period survived the sweep")
	}
	if !m.known(listening, "") {
		t.Fatal("a connection with an open stream was swept")
	}
	if !m.known(insideGrace, "") {
		t.Fatal("a connection still inside its grace was swept")
	}
}
