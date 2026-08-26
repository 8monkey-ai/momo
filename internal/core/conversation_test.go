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

// Sent runs the same course as Received, so a test reads the calls of both routes
// in one order.
func (h *held) Sent(ctx context.Context, m Message) { _ = h.Received(ctx, m, nil) }

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
		s, _ := Serialize(discard(), h, nil)

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
		s, _ := Serialize(discard(), h, nil)

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
		s, _ := Serialize(discard(), h, nil)

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
		s, _ := Serialize(discard(), h, nil)

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
		s, _ := Serialize(discard(), h, nil)

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
		s, _ := Serialize(discard(), h, nil)

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
		s, _ := Serialize(discard(), h, nil)
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
		s, _ := Serialize(discard(), h, nil)
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
		s, _ := Serialize(discard(), h, nil)

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
		s, _ := Serialize(discard(), h, nil)

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
		s, _ := Serialize(discard(), d.Handler(NewHandler(discard(), a)), nil)

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

func record(ctx context.Context, h Handler, conversation, text string) <-chan error {
	return arrive(ctx, h, conversation, text, nil)
}

func TestNoRecordsRouteWithoutARecordingHandler(t *testing.T) {
	if _, records := Serialize(discard(), newHeld(), nil); records != nil {
		t.Fatalf("Serialize answered %+v as the records route, want none", records)
	}
}

func TestARecordWaitsForTheTurnOfItsConversation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		turns, records := newHeld(), newHeld()
		s, r := Serialize(discard(), turns, records)

		turn := arrive(t.Context(), s, "stub:1", "one", nil)
		<-turns.entered
		recorded := record(t.Context(), r, "stub:1", "/user-message two")
		synctest.Wait()
		wantPrompts(t, records)

		turns.release <- struct{}{}
		<-records.entered
		records.release <- struct{}{}
		if err := <-turn; err != nil {
			t.Fatalf("the turn returned %v", err)
		}
		if err := <-recorded; err != nil {
			t.Fatalf("the record returned %v", err)
		}
		wantPrompts(t, records, "/user-message two")
	})
}

func TestAnOutgoingRecordWaitsForTheTurnOfItsConversation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		turns, records := newHeld(), newHeld()
		s, r := Serialize(discard(), turns, records)

		turn := arrive(t.Context(), s, "stub:1", "one", nil)
		<-turns.entered
		recorded := make(chan struct{})
		go func() {
			defer close(recorded)
			r.Sent(t.Context(), Message{Conversation: "stub:1", Content: Text("/assistant-message two")})
		}()
		synctest.Wait()
		wantPrompts(t, records)

		turns.release <- struct{}{}
		<-records.entered
		records.release <- struct{}{}
		<-recorded
		if err := <-turn; err != nil {
			t.Fatalf("the turn returned %v", err)
		}
		wantPrompts(t, records, "/assistant-message two")
	})
}

// A record carries no prompt of the contact, so the waiting message keeps a turn of
// its own instead of being merged into the record.
func TestATurnWaitsForTheRecordOfItsConversation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		turns, records := newHeld(), newHeld()
		s, r := Serialize(discard(), turns, records)
		answered := &recorder{}

		recorded := record(t.Context(), r, "stub:1", "/user-message one")
		<-records.entered
		turn := arrive(t.Context(), s, "stub:1", "two", answered.reply)
		synctest.Wait()
		wantPrompts(t, turns)

		records.release <- struct{}{}
		<-turns.entered
		turns.release <- struct{}{}
		if err := <-recorded; err != nil {
			t.Fatalf("the record returned %v", err)
		}
		if err := <-turn; err != nil {
			t.Fatalf("the turn returned %v", err)
		}
		wantPrompts(t, turns, "two")
		want(t, answered, "answer to two")
	})
}

func TestTwoRecordsOfOneConversationRunOneAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		records := newHeld()
		_, r := Serialize(discard(), newHeld(), records)

		first := record(t.Context(), r, "stub:1", "one")
		<-records.entered
		second := record(t.Context(), r, "stub:1", "two")
		synctest.Wait()
		wantPrompts(t, records, "one")

		records.release <- struct{}{}
		<-records.entered
		records.release <- struct{}{}
		if err := <-first; err != nil {
			t.Fatalf("the first record returned %v", err)
		}
		if err := <-second; err != nil {
			t.Fatalf("the second record returned %v", err)
		}
		wantPrompts(t, records, "one", "two")
	})
}

// TestRecordsOfTwoConversationsRunAtTheSameTime releases nothing before both
// records are inside the handler, so an implementation with one lock for every
// conversation fails by deadlock and not by a measurement of time.
func TestRecordsOfTwoConversationsRunAtTheSameTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		records := newHeld()
		_, r := Serialize(discard(), newHeld(), records)

		one := record(t.Context(), r, "stub:1", "one")
		two := record(t.Context(), r, "stub:2", "two")
		<-records.entered
		<-records.entered

		records.release <- struct{}{}
		records.release <- struct{}{}
		if err := <-one; err != nil {
			t.Fatalf("the first record returned %v", err)
		}
		if err := <-two; err != nil {
			t.Fatalf("the second record returned %v", err)
		}
	})
}

// releaseAll lets n held calls finish, the one inside the handler first: the test
// has already read the entry of that call.
func releaseAll(h *held, n int) {
	for i := range n {
		if i > 0 {
			<-h.entered
		}
		h.release <- struct{}{}
	}
}

// queued starts a call and waits until it is queued, so the order the calls
// entered Serialize is the order of the arguments.
func queued(ctx context.Context, h Handler, conversation, text string) <-chan error {
	done := arrive(ctx, h, conversation, text, nil)
	synctest.Wait()
	return done
}

// wantOrder releases every held call and states the order the handler ran them in.
// Every test that uses it queues several calls, because two callers that race for
// the conversation land in the right order often enough to pass by luck.
func wantOrder(t *testing.T, h *held, done []<-chan error, prompts ...string) {
	t.Helper()
	releaseAll(h, len(done))
	for _, failed := range done {
		if err := <-failed; err != nil {
			t.Fatalf("a call returned %v", err)
		}
	}
	wantPrompts(t, h, prompts...)
}

// The order the agent's session depends on: what waits behind a turn reaches the
// agent as it was said.
func TestQueuedRecordsRunInTheOrderTheyEntered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s, r := Serialize(discard(), h, h)

		done := []<-chan error{arrive(t.Context(), s, "stub:1", "turn", nil)}
		<-h.entered
		for _, text := range []string{"one", "two", "three", "four", "five"} {
			done = append(done, queued(t.Context(), r, "stub:1", text))
		}

		wantOrder(t, h, done, "turn", "one", "two", "three", "four", "five")
	})
}

// A turn does not overtake a record that entered before it.
func TestARecordAndATurnQueuedBehindARecordKeepTheirOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s, r := Serialize(discard(), h, h)

		done := []<-chan error{record(t.Context(), r, "stub:1", "held")}
		<-h.entered
		for _, text := range []string{"one", "three", "five"} {
			done = append(done, queued(t.Context(), r, "stub:1", text))
			done = append(done, queued(t.Context(), s, "stub:1", text+" answered"))
		}

		wantOrder(t, h, done, "held", "one", "one answered", "three", "three answered", "five", "five answered")
	})
}

// A record does not overtake a turn that entered before it.
func TestATurnAndARecordQueuedBehindARecordKeepTheirOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s, r := Serialize(discard(), h, h)

		done := []<-chan error{record(t.Context(), r, "stub:1", "held")}
		<-h.entered
		for _, text := range []string{"one", "three", "five"} {
			done = append(done, queued(t.Context(), s, "stub:1", text+" answered"))
			done = append(done, queued(t.Context(), r, "stub:1", text))
		}

		wantOrder(t, h, done, "held", "one answered", "one", "three answered", "three", "five answered", "five")
	})
}

// A caller that gave up neither holds the conversation nor changes the order of
// what is left.
func TestACancelledQueuedCallerLeavesTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHeld()
		s, r := Serialize(discard(), h, h)
		ctx, cancel := context.WithCancel(t.Context())

		held := record(t.Context(), r, "stub:1", "held")
		<-h.entered
		gone := queued(ctx, r, "stub:1", "gone")
		last := queued(t.Context(), s, "stub:1", "last")

		cancel()
		if err := <-gone; !errors.Is(err, context.Canceled) {
			t.Fatalf("the queued caller returned %v, want %v", err, context.Canceled)
		}

		wantOrder(t, h, []<-chan error{held, last}, "held", "last")
	})
}

func TestTheConversationIsForgottenAfterARecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		records := newHeld()
		s, r := Serialize(discard(), newHeld(), records)

		done := record(t.Context(), r, "stub:1", "one")
		<-records.entered
		records.release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatalf("the record returned %v", err)
		}

		c := s.(*conversations)
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.state) != 0 {
			t.Fatalf("the map holds %d conversations, want none", len(c.state))
		}
	})
}
