package channel

import (
	"context"
	"errors"
	"testing"

	"github.com/8monkey-ai/momo/internal/core"
)

func noSettings(any) error { return nil }

func collect(seen chan core.Message) core.Handler { return sink{seen: seen} }

type sink struct {
	seen chan core.Message
}

func (s sink) Received(_ context.Context, m core.Message, _ core.Reply) error {
	s.seen <- m
	return nil
}

func (s sink) Sent(_ context.Context, m core.Message) { s.seen <- m }

func stub(name string) Factory {
	return func(context.Context, Decoder, core.Handler) (Channel, error) {
		return fixed{routes: []Route{{Path: "/" + name}}}, nil
	}
}

type fixed struct {
	routes []Route
}

func (f fixed) Routes() []Route { return f.routes }

func isolateFactories(t *testing.T) {
	t.Helper()
	saved := factories
	factories = map[string]Factory{}
	t.Cleanup(func() { factories = saved })
}

func TestBuildsRegisteredChannelsInAStableOrder(t *testing.T) {
	isolateFactories(t)
	Register("stub-b", stub("b"))
	Register("stub-a", stub("a"))

	got, err := Build(context.Background(), map[string]Decoder{"stub-b": noSettings, "stub-a": noSettings}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) != 2 || got[0].Name != "stub-a" || got[1].Name != "stub-b" {
		t.Fatalf("instances = %+v, want stub-a then stub-b", got)
	}
}

// TestBuildQualifiesMessagesWithTheChannelName pins what keeps two channels
// apart: the same contact id on two channels is two conversations, and no
// channel names itself.
func TestBuildQualifiesMessagesWithTheChannelName(t *testing.T) {
	isolateFactories(t)
	seen := make(chan core.Message, 2)
	Register("stub-a", record("123"))
	Register("stub-b", record("123"))

	if _, err := Build(context.Background(), map[string]Decoder{"stub-a": noSettings, "stub-b": noSettings}, collect(seen)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	first, second := (<-seen).Contact, (<-seen).Contact
	if first != "stub-a:123" || second != "stub-b:123" {
		t.Fatalf("contacts = %q and %q, want each qualified with its channel", first, second)
	}
}

// record is a factory that delivers one message with the given contact id as it
// is built, so the test sees what the handler received.
func record(contact string) Factory {
	return func(lifetime context.Context, _ Decoder, h core.Handler) (Channel, error) {
		h.Sent(lifetime, core.Message{Contact: contact})
		return fixed{}, nil
	}
}

func TestBuildRejectsUnconfiguredChannelName(t *testing.T) {
	if _, err := Build(context.Background(), map[string]Decoder{"telegran": noSettings}, nil); err == nil {
		t.Fatal("Build succeeded, want an error naming the unknown channel")
	}
}

func TestBuildReportsWhichChannelFailed(t *testing.T) {
	isolateFactories(t)
	broken := errors.New("missing signing key")
	Register("stub-broken", func(context.Context, Decoder, core.Handler) (Channel, error) { return nil, broken })

	_, err := Build(context.Background(), map[string]Decoder{"stub-broken": noSettings}, nil)
	if !errors.Is(err, broken) {
		t.Fatalf("error = %v, want it to wrap %v", err, broken)
	}
}
