package channel

import (
	"context"
	"errors"
	"testing"

	"github.com/8monkey-ai/momo/internal/core"
)

func noSettings(any) error { return nil }

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

// deliver is a channel that hands one message to the handler it was built with,
// in both directions, so a test can observe what the handler sees.
func deliver(conversation string) Factory {
	return func(_ context.Context, _ Decoder, h core.Handler) (Channel, error) {
		m := core.Message{Conversation: conversation, Content: core.Text("hello")}
		h.Received(context.Background(), m, func(context.Context, []core.ContentBlock) error { return nil })
		h.Sent(context.Background(), m)
		return fixed{}, nil
	}
}

type recorder struct {
	received []string
	sent     []string
}

func (r *recorder) Received(_ context.Context, m core.Message, _ core.Reply) {
	r.received = append(r.received, m.Conversation)
}

func (r *recorder) Sent(_ context.Context, m core.Message) {
	r.sent = append(r.sent, m.Conversation)
}

func TestHandlerSeesTheConversationQualifiedWithTheChannelName(t *testing.T) {
	isolateFactories(t)
	Register("respondio", deliver("123"))
	Register("acp", deliver("123"))
	got := &recorder{}

	if _, err := Build(context.Background(), map[string]Decoder{"respondio": noSettings, "acp": noSettings}, got); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.received) != 2 || got.received[0] != "acp:123" || got.received[1] != "respondio:123" {
		t.Fatalf("received = %v, want [acp:123 respondio:123]", got.received)
	}
}

func TestSentIsQualifiedWithTheChannelName(t *testing.T) {
	isolateFactories(t)
	Register("respondio", deliver("123"))
	got := &recorder{}

	if _, err := Build(context.Background(), map[string]Decoder{"respondio": noSettings}, got); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.sent) != 1 || got.sent[0] != "respondio:123" {
		t.Fatalf("sent = %v, want [respondio:123]", got.sent)
	}
}

func TestChannelCannotSupplyTheChannelPartItself(t *testing.T) {
	isolateFactories(t)
	Register("respondio", deliver("acp:123"))
	got := &recorder{}

	if _, err := Build(context.Background(), map[string]Decoder{"respondio": noSettings}, got); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.received) != 1 || got.received[0] != "respondio:acp:123" {
		t.Fatalf("received = %v, want [respondio:acp:123]", got.received)
	}
}
