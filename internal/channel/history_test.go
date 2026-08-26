package channel

import (
	"context"
	"testing"

	"github.com/8monkey-ai/momo/internal/core"
)

type recordingHistory struct {
	conversations chan string
}

func (h recordingHistory) RecordUser(_ context.Context, m core.Message) {
	h.conversations <- m.Conversation
}

func (h recordingHistory) RecordAssistant(context.Context, core.Message) {}

// A record carries the channel's name, so a record and a turn of one conversation
// are one conversation for the agent.
func TestBuildHandsTheHistoryToEveryChannel(t *testing.T) {
	isolateFactories(t)
	sync := recordingHistory{conversations: make(chan string, 1)}
	var got core.History
	Register("stub", func(_ context.Context, _ Decoder, _ core.Handler, h core.History) (Channel, error) {
		got = h
		return fixed{}, nil
	})
	if _, err := Build(context.Background(), configured("stub"), nil, sync); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got == nil {
		t.Fatal("the channel got no history, want the configured one")
	}
	got.RecordUser(context.Background(), core.Message{Conversation: "123"})
	if recorded := <-sync.conversations; recorded != "stub:123" {
		t.Fatalf("recorded conversation = %q, want \"stub:123\"", recorded)
	}
}

// A deployment without human handover is valid, and a channel that needs the
// extension refuses its own configuration at start-up.
func TestAChannelBuiltWithoutTheExtensionGetsNoHistory(t *testing.T) {
	isolateFactories(t)
	var got core.History
	var built bool
	Register("stub", func(_ context.Context, _ Decoder, _ core.Handler, h core.History) (Channel, error) {
		got, built = h, true
		return fixed{}, nil
	})
	if _, err := Build(context.Background(), configured("stub"), nil, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !built {
		t.Fatal("the channel was never built")
	}
	if got != nil {
		t.Fatalf("the channel got history %+v, want none", got)
	}
}
