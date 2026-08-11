package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// agentFunc is an agent whose whole behaviour is the function it is given.
type agentFunc func(ctx context.Context, conversation string, prompt []ContentBlock) ([]ContentBlock, error)

func (f agentFunc) Turn(ctx context.Context, conversation string, prompt []ContentBlock) ([]ContentBlock, error) {
	return f(ctx, conversation, prompt)
}

func collect(t *testing.T) (Reply, func() []ContentBlock) {
	t.Helper()
	var mu sync.Mutex
	var got []ContentBlock
	reply := func(_ context.Context, content []ContentBlock) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, content...)
		return nil
	}
	return reply, func() []ContentBlock {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func TestHandlerDeliversWhatTheAgentAnswered(t *testing.T) {
	h := &AgentHandler{Log: discard(), Agent: agentFunc(
		func(_ context.Context, conversation string, prompt []ContentBlock) ([]ContentBlock, error) {
			return Text(conversation + " said " + TextOf(prompt)), nil
		})}
	reply, delivered := collect(t)

	h.Received(context.Background(), Message{Contact: "respondio:7", Content: Text("hello")}, reply)

	if got := TextOf(delivered()); got != "respondio:7 said hello" {
		t.Fatalf("delivered %q, want the agent's reply for the qualified conversation", got)
	}
}

func TestHandlerDeliversNothingWhenTheTurnFails(t *testing.T) {
	h := &AgentHandler{Log: discard(), Agent: agentFunc(
		func(context.Context, string, []ContentBlock) ([]ContentBlock, error) {
			return nil, errors.New("the harness died")
		})}
	reply, delivered := collect(t)

	h.Received(context.Background(), Message{Contact: "respondio:7", Content: Text("hello")}, reply)

	if got := delivered(); got != nil {
		t.Fatalf("delivered %v, want nothing", got)
	}
}

func TestHandlerRunsOneTurnAtATimePerConversation(t *testing.T) {
	var mu sync.Mutex
	live, overlaps := 0, 0
	h := &AgentHandler{Log: discard(), Agent: agentFunc(
		func(_ context.Context, _ string, prompt []ContentBlock) ([]ContentBlock, error) {
			mu.Lock()
			live++
			if live > 1 {
				overlaps++
			}
			mu.Unlock()
			runtime.Gosched()
			mu.Lock()
			live--
			mu.Unlock()
			return prompt, nil
		})}
	reply, delivered := collect(t)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Received(context.Background(), Message{Contact: "respondio:7", Content: Text("hi")}, reply)
		}()
	}
	wg.Wait()

	if overlaps != 0 {
		t.Errorf("%d turns overlapped, want the conversation's turns serialized", overlaps)
	}
	if got := len(delivered()); got != 50 {
		t.Errorf("delivered %d replies, want 50", got)
	}
}

func TestHandlerRunsDifferentConversationsConcurrently(t *testing.T) {
	// Both turns must be inside the agent at once: an implementation that
	// serializes unrelated conversations never lets the second one in, and the
	// test fails by timing out.
	both := make(chan struct{})
	var once sync.Once
	arrived := 0
	var mu sync.Mutex
	h := &AgentHandler{Log: discard(), Agent: agentFunc(
		func(_ context.Context, _ string, prompt []ContentBlock) ([]ContentBlock, error) {
			mu.Lock()
			arrived++
			if arrived == 2 {
				once.Do(func() { close(both) })
			}
			mu.Unlock()
			<-both
			return prompt, nil
		})}
	reply, _ := collect(t)

	var wg sync.WaitGroup
	for _, contact := range []string{"respondio:1", "acp:1"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Received(context.Background(), Message{Contact: contact, Content: Text("hi")}, reply)
		}()
	}
	wg.Wait()
}
