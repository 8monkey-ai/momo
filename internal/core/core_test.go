package core

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"
)

type recorder struct {
	received []Message
	sent     []Message
	err      error
}

func (r *recorder) Received(_ context.Context, m Message, _ Reply) error {
	r.received = append(r.received, m)
	return r.err
}

func (r *recorder) Sent(_ context.Context, m Message) { r.sent = append(r.sent, m) }

type stubAgent struct {
	conversation string
	prompt       []ContentBlock
	content      []ContentBlock
	err          error
}

func (s *stubAgent) Turn(_ context.Context, conversation string, prompt []ContentBlock) ([]ContentBlock, error) {
	s.conversation = conversation
	s.prompt = prompt
	return s.content, s.err
}

func TestQualifyNamesEveryMessageWithTheChannelItArrivedOn(t *testing.T) {
	inner := &recorder{}
	h := Qualify("respondio", inner)

	if err := h.Received(context.Background(), Message{Contact: "12345"}, nil); err != nil {
		t.Fatalf("Received: %v", err)
	}
	h.Sent(context.Background(), Message{Contact: "12345"})

	if got := inner.received[0].Contact; got != "respondio:12345" {
		t.Errorf("received contact = %q, want %q", got, "respondio:12345")
	}
	if got := inner.sent[0].Contact; got != "respondio:12345" {
		t.Errorf("sent contact = %q, want %q", got, "respondio:12345")
	}
}

func TestQualifyPassesTheTurnFailureOn(t *testing.T) {
	failed := errors.New("the harness stopped")
	h := Qualify("acp", &recorder{err: failed})
	if err := h.Received(context.Background(), Message{Contact: "1"}, nil); !errors.Is(err, failed) {
		t.Fatalf("error = %v, want it to wrap %v", err, failed)
	}
}

func TestAgentHandlerSendsTheWholeReplyOfOneTurn(t *testing.T) {
	a := &stubAgent{content: []ContentBlock{{Type: "text", Text: "part one"}, {Type: "text", Text: "part two"}}}
	var sent [][]ContentBlock
	h := AgentHandler{Agent: a, Log: slog.New(slog.DiscardHandler)}

	err := h.Received(context.Background(), Message{Contact: "respondio:1", Content: Text("hello")},
		func(_ context.Context, content []ContentBlock) error {
			sent = append(sent, content)
			return nil
		})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	if a.conversation != "respondio:1" {
		t.Errorf("conversation = %q, want %q", a.conversation, "respondio:1")
	}
	if !reflect.DeepEqual(a.prompt, Text("hello")) {
		t.Errorf("prompt = %+v, want the message content", a.prompt)
	}
	if len(sent) != 1 {
		t.Fatalf("the reply was sent %d times, want once", len(sent))
	}
	if !reflect.DeepEqual(sent[0], a.content) {
		t.Errorf("reply = %+v, want %+v", sent[0], a.content)
	}
}

func TestAgentHandlerReportsAFailedTurnAndSendsNothing(t *testing.T) {
	failed := errors.New("the harness stopped")
	h := AgentHandler{Agent: &stubAgent{err: failed}, Log: slog.New(slog.DiscardHandler)}

	err := h.Received(context.Background(), Message{Contact: "respondio:1", Content: Text("hello")},
		func(context.Context, []ContentBlock) error {
			t.Error("the reply was sent for a failed turn")
			return nil
		})
	if !errors.Is(err, failed) {
		t.Fatalf("error = %v, want it to wrap %v", err, failed)
	}
}

func TestAgentHandlerReportsAReplyThatCouldNotBeSent(t *testing.T) {
	undelivered := errors.New("nothing is listening")
	h := AgentHandler{Agent: &stubAgent{content: Text("hi")}, Log: slog.New(slog.DiscardHandler)}

	err := h.Received(context.Background(), Message{Contact: "acp:1", Content: Text("hello")},
		func(context.Context, []ContentBlock) error { return undelivered })
	if !errors.Is(err, undelivered) {
		t.Fatalf("error = %v, want it to wrap %v", err, undelivered)
	}
}
