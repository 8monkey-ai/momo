package core

import (
	"context"
	"testing"
)

type capture struct {
	received chan Message
	sent     chan Message
}

func newCapture() capture {
	return capture{received: make(chan Message, 1), sent: make(chan Message, 1)}
}

func (c capture) Received(_ context.Context, m Message, _ Reply) { c.received <- m }
func (c capture) Sent(_ context.Context, m Message)              { c.sent <- m }

func TestQualifyNamesTheChannelAMessageArrivedOn(t *testing.T) {
	c := newCapture()
	h := Qualify("respondio", c)

	h.Received(context.Background(), Message{Contact: "123"}, nil)
	if got := (<-c.received).Contact; got != "respondio:123" {
		t.Errorf("received contact = %q, want %q", got, "respondio:123")
	}
	h.Sent(context.Background(), Message{Contact: "123"})
	if got := (<-c.sent).Contact; got != "respondio:123" {
		t.Errorf("sent contact = %q, want %q", got, "respondio:123")
	}
}
