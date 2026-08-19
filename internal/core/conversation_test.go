package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// held is a Handler that records every call and holds it until the test releases
// it. It replies once with the text of the prompt it was given, so a test reads
// which Reply the turn was delivered on.
type held struct {
	entered chan struct{}
	release chan struct{}

	mu    sync.Mutex
	calls []Message
	errs  []error
}

func newHeld(errs ...error) *held {
	return &held{entered: make(chan struct{}, 8), release: make(chan struct{}, 8), errs: errs}
}

func (h *held) Received(ctx context.Context, m Message, reply Reply) error {
	h.mu.Lock()
	n := len(h.calls)
	h.calls = append(h.calls, m)
	h.mu.Unlock()
	h.entered <- struct{}{}
	<-h.release
	if reply != nil {
		_ = reply(ctx, Text("answer to "+TextOf(m.Content)))
	}
	if n < len(h.errs) {
		return h.errs[n]
	}
	return nil
}

func (h *held) Sent(context.Context, Message) {}

// prompts answers the text of every call's content blocks, so a test states a
// merged prompt as literals.
func (h *held) prompts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.calls))
	for _, m := range h.calls {
		texts := make([]string, 0, len(m.Content))
		for _, block := range m.Content {
			texts = append(texts, block.Text)
		}
		out = append(out, strings.Join(texts, "+"))
	}
	return out
}

func wantPrompts(t *testing.T, h *held, prompts ...string) {
	t.Helper()
	got := h.prompts()
	if strings.Join(got, "|") != strings.Join(prompts, "|") {
		t.Fatalf("the handler saw %q, want %q", got, prompts)
	}
}

// arrive delivers one message on a goroutine of its own and answers what
// Received returned.
func arrive(ctx context.Context, h Handler, conversation, text string, reply Reply) <-chan error {
	failed := make(chan error, 1)
	go func() {
		failed <- h.Received(ctx, Message{Conversation: conversation, Content: Text(text)}, reply)
	}()
	return failed
}

func TestASecondMessageStartsNoTurnWhileTheFirstRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s := Serialize(discard(), h)

		first := arrive(t.Context(), s, "stub:1", "one", nil)
		<-h.entered
		second := arrive(t.Context(), s, "stub:1", "two", nil)
		synctest.Wait()
		wantPrompts(t, h, "one")

		h.release <- struct{}{}
		<-h.entered
		h.release <- struct{}{}
		if err := <-first; err != nil {
			t.Fatalf("the first caller returned %v", err)
		}
		if err := <-second; err != nil {
			t.Fatalf("the second caller returned %v", err)
		}
	})
}

func TestTwoMessagesDuringATurnBecomeOnePrompt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s := Serialize(discard(), h)

		first := arrive(t.Context(), s, "stub:1", "one", nil)
		<-h.entered
		second := arrive(t.Context(), s, "stub:1", "two", nil)
		synctest.Wait()
		third := arrive(t.Context(), s, "stub:1", "three", nil)
		synctest.Wait()

		h.release <- struct{}{}
		<-h.entered
		h.release <- struct{}{}
		for _, failed := range []<-chan error{first, second, third} {
			if err := <-failed; err != nil {
				t.Fatalf("a caller returned %v", err)
			}
		}
		wantPrompts(t, h, "one", "two+three")
	})
}

// TestAMessageArrivingDuringAMergedTurnGetsATurnOfItsOwn pins that batches
// repeat: the conversation keeps running turns until nothing is pending.
func TestAMessageArrivingDuringAMergedTurnGetsATurnOfItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s := Serialize(discard(), h)

		first := arrive(t.Context(), s, "stub:1", "one", nil)
		<-h.entered
		second := arrive(t.Context(), s, "stub:1", "two", nil)
		synctest.Wait()

		h.release <- struct{}{}
		<-h.entered
		third := arrive(t.Context(), s, "stub:1", "three", nil)
		synctest.Wait()

		h.release <- struct{}{}
		<-h.entered
		h.release <- struct{}{}
		for _, failed := range []<-chan error{first, second, third} {
			if err := <-failed; err != nil {
				t.Fatalf("a caller returned %v", err)
			}
		}
		wantPrompts(t, h, "one", "two", "three")
	})
}

// TestTurnsOfTwoConversationsRunAtTheSameTime releases nothing before both turns
// are inside the handler, so an implementation that serialises them fails by
// deadlock and not by a measurement of time.
func TestTurnsOfTwoConversationsRunAtTheSameTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s := Serialize(discard(), h)

		one := arrive(t.Context(), s, "stub:1", "one", nil)
		two := arrive(t.Context(), s, "stub:2", "two", nil)
		<-h.entered
		<-h.entered

		h.release <- struct{}{}
		h.release <- struct{}{}
		if err := <-one; err != nil {
			t.Fatalf("the first caller returned %v", err)
		}
		if err := <-two; err != nil {
			t.Fatalf("the second caller returned %v", err)
		}
	})
}

func TestEachCallerReturnsTheErrorOfTheTurnThatCarriedItsMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		own := errors.New("the first turn failed")
		merged := errors.New("the merged turn failed")
		h := newHeld(own, merged)
		s := Serialize(discard(), h)

		first := arrive(t.Context(), s, "stub:1", "one", nil)
		<-h.entered
		second := arrive(t.Context(), s, "stub:1", "two", nil)
		synctest.Wait()

		h.release <- struct{}{}
		<-h.entered
		synctest.Wait()
		select {
		case err := <-second:
			t.Fatalf("the second caller returned %v before the merged turn ended", err)
		default:
		}

		h.release <- struct{}{}
		if err := <-first; !errors.Is(err, own) {
			t.Fatalf("the first caller returned %v, want %v", err, own)
		}
		if err := <-second; !errors.Is(err, merged) {
			t.Fatalf("the second caller returned %v, want %v", err, merged)
		}
	})
}

func TestAFailedTurnStillRunsTheBatchBehindIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld(errors.New("the agent exited"))
		s := Serialize(discard(), h)

		first := arrive(t.Context(), s, "stub:1", "one", nil)
		<-h.entered
		second := arrive(t.Context(), s, "stub:1", "two", nil)
		synctest.Wait()

		h.release <- struct{}{}
		<-h.entered
		h.release <- struct{}{}
		<-first
		if err := <-second; err != nil {
			t.Fatalf("the merged turn returned %v", err)
		}
		wantPrompts(t, h, "one", "two")
	})
}

// TestTheMergedReplyIsDeliveredOnTheRunningCallersReply pins which transport
// carries a merged reply: the one of the caller that is inside Received, because
// a waiting caller may have given up.
func TestTheMergedReplyIsDeliveredOnTheRunningCallersReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s := Serialize(discard(), h)
		running, waiting := &recorder{}, &recorder{}

		first := arrive(t.Context(), s, "stub:1", "one", running.reply)
		<-h.entered
		second := arrive(t.Context(), s, "stub:1", "two", waiting.reply)
		synctest.Wait()

		h.release <- struct{}{}
		<-h.entered
		h.release <- struct{}{}
		<-first
		<-second
		want(t, running, "answer to one", "answer to two")
		if waiting.count() != 0 {
			t.Fatalf("the waiting caller's reply delivered %q, want nothing", waiting.texts())
		}
	})
}

func TestACancelledWaiterGivesUpAndItsMessageIsStillSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s := Serialize(discard(), h)
		ctx, cancel := context.WithCancel(t.Context())

		first := arrive(t.Context(), s, "stub:1", "one", nil)
		<-h.entered
		second := arrive(ctx, s, "stub:1", "two", nil)
		synctest.Wait()

		cancel()
		if err := <-second; !errors.Is(err, context.Canceled) {
			t.Fatalf("the waiting caller returned %v, want %v", err, context.Canceled)
		}

		h.release <- struct{}{}
		<-h.entered
		h.release <- struct{}{}
		<-first
		wantPrompts(t, h, "one", "two")
	})
}

func TestTheConversationIsForgottenWhenItGoesQuiet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s := Serialize(discard(), h)

		first := arrive(t.Context(), s, "stub:1", "one", nil)
		<-h.entered
		second := arrive(t.Context(), s, "stub:1", "two", nil)
		synctest.Wait()
		h.release <- struct{}{}
		<-h.entered
		h.release <- struct{}{}
		<-first
		<-second

		c := s.(*conversations)
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.state) != 0 {
			t.Fatalf("the map holds %d conversations, want none", len(c.state))
		}
	})
}

func TestAMessageAfterTheConversationWentQuietStartsItsOwnTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s := Serialize(discard(), h)

		for _, text := range []string{"one", "two"} {
			failed := arrive(t.Context(), s, "stub:1", text, nil)
			<-h.entered
			h.release <- struct{}{}
			if err := <-failed; err != nil {
				t.Fatalf("the caller returned %v", err)
			}
		}
		wantPrompts(t, h, "one", "two")
	})
}

// agentFunc is an agent whose turn answers the prompt it was given.
type agentFunc func(m Message, emit Emit) error

func (f agentFunc) Turn(_ context.Context, m Message, emit Emit) error { return f(m, emit) }

// TestAMergedTurnStartsAfterTheLastParagraphWasDelivered holds the two rules
// together: the conversation is busy until the paced delivery of its reply ended,
// so no reply is interleaved with another.
func TestAMergedTurnStartsAfterTheLastParagraphWasDelivered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var events []string
		record := func(name string) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, name)
		}
		reply := func(_ context.Context, content []ContentBlock) error {
			record("sent " + TextOf(content))
			return nil
		}
		a := agentFunc(func(m Message, emit Emit) error {
			record("turn " + TextOf(m.Content))
			if TextOf(m.Content) == "hi" {
				return emit(Text("first\n\nsecond\n\n"))
			}
			return emit(Text("answer\n\n"))
		})
		d := Delivery{Separator: "\n\n", WordsPerMinute: 60, MaxDelay: time.Minute}
		s := Serialize(discard(), d.Handler(NewHandler(discard(), a)))

		first := arrive(t.Context(), s, "stub:1", "hi", reply)
		// The first paragraph is paced for a second, and the second for one more.
		time.Sleep(1500 * time.Millisecond)
		second := arrive(t.Context(), s, "stub:1", "more", reply)
		<-first
		<-second

		mu.Lock()
		defer mu.Unlock()
		wanted := []string{"turn hi", "sent first", "sent second", "turn more", "sent answer"}
		if strings.Join(events, "|") != strings.Join(wanted, "|") {
			t.Fatalf("events = %q, want %q", events, wanted)
		}
	})
}
