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

// capture is a handler that keeps the message it was handed.
type capture struct {
	message core.Message
}

func (c *capture) Received(_ context.Context, m core.Message, _ core.Reply) { c.message = m }
func (c *capture) Sent(_ context.Context, m core.Message)                   { c.message = m }

func TestAMessageCarriesTheChannelItArrivedOnInItsConversation(t *testing.T) {
	isolateFactories(t)
	var handed core.Handler
	Register("stub-c", func(_ context.Context, _ Decoder, h core.Handler) (Channel, error) {
		handed = h
		return fixed{}, nil
	})
	seen := &capture{}
	if _, err := Build(context.Background(), map[string]Decoder{"stub-c": noSettings}, seen); err != nil {
		t.Fatalf("Build: %v", err)
	}

	handed.Received(context.Background(), core.Message{Conversation: "123"}, nil)

	if seen.message.Conversation != "stub-c:123" {
		t.Fatalf("conversation = %q, want \"stub-c:123\"", seen.message.Conversation)
	}
}

func TestAnOutgoingMessageIsQualifiedTheSameWay(t *testing.T) {
	isolateFactories(t)
	var handed core.Handler
	Register("stub-d", func(_ context.Context, _ Decoder, h core.Handler) (Channel, error) {
		handed = h
		return fixed{}, nil
	})
	seen := &capture{}
	if _, err := Build(context.Background(), map[string]Decoder{"stub-d": noSettings}, seen); err != nil {
		t.Fatalf("Build: %v", err)
	}

	handed.Sent(context.Background(), core.Message{Conversation: "456"})

	if seen.message.Conversation != "stub-d:456" {
		t.Fatalf("conversation = %q, want \"stub-d:456\"", seen.message.Conversation)
	}
}
