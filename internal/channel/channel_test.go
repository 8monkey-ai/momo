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

type capture struct {
	contacts chan string
}

func (c capture) Received(_ context.Context, m core.Message, _ core.Reply) { c.contacts <- m.Contact }
func (c capture) Sent(_ context.Context, m core.Message)                   { c.contacts <- m.Contact }

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

func TestBuiltChannelsSpeakToAQualifiedConversation(t *testing.T) {
	isolateFactories(t)
	contacts := make(chan string, 1)
	Register("stub-a", func(_ context.Context, _ Decoder, h core.Handler) (Channel, error) {
		h.Received(context.Background(), core.Message{Contact: "123"}, nil)
		return fixed{}, nil
	})

	if _, err := Build(context.Background(), map[string]Decoder{"stub-a": noSettings}, capture{contacts}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := <-contacts; got != "stub-a:123" {
		t.Fatalf("contact = %q, want it qualified with the channel's configured name", got)
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
