package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is the fake Reply every delivery test sends through: it records each
// call, and fails the call the test asks it to fail.
type recorder struct {
	failOn int

	mu    sync.Mutex
	calls [][]ContentBlock
	seen  chan struct{}
}

func newRecorder() *recorder {
	return &recorder{failOn: -1, seen: make(chan struct{}, 16)}
}

func (r *recorder) reply(_ context.Context, content []ContentBlock) error {
	r.mu.Lock()
	r.calls = append(r.calls, content)
	count := len(r.calls)
	r.mu.Unlock()
	r.seen <- struct{}{}
	if count == r.failOn {
		return errors.New("the channel refused the message")
	}
	return nil
}

func (r *recorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	texts := make([]string, 0, len(r.calls))
	for _, content := range r.calls {
		texts = append(texts, TextOf(content))
	}
	return texts
}

func (r *recorder) waitForCall(t *testing.T) {
	t.Helper()
	select {
	case <-r.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no Reply call arrived")
	}
}

// generator is an agent whose turn is the function the test supplies.
type generator func(emit Emit) error

func (g generator) Turn(_ context.Context, _ Message, emit Emit) error { return g(emit) }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// instant is the pacing every ordering test runs with: no pause, paragraphs
// closed by a blank line.
func instant() Pacing { return Pacing{MaxDelay: time.Minute, Separator: "\n\n"} }

func received(t *testing.T, ctx context.Context, p Pacing, r *recorder, g generator) error {
	t.Helper()
	return NewHandler(discard(), g, p).Received(ctx, Message{Conversation: "test:1"}, r.reply)
}

func wantTexts(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reply calls = %q, want %q", got, want)
	}
}

func TestPauseIsProportionalToTheWordCount(t *testing.T) {
	got := pause("one two three", Pacing{DelayPerWord: 100 * time.Millisecond, MaxDelay: time.Minute})
	if got != 300*time.Millisecond {
		t.Errorf("pause = %v, want 300ms", got)
	}
}

func TestPauseIsCappedByMaxDelay(t *testing.T) {
	got := pause("one two three", Pacing{DelayPerWord: 100 * time.Millisecond, MaxDelay: 250 * time.Millisecond})
	if got != 250*time.Millisecond {
		t.Errorf("pause = %v, want 250ms", got)
	}
}

func TestTheQueueWaitsBeforeItDelivers(t *testing.T) {
	r := newRecorder()
	pacing := Pacing{DelayPerWord: 50 * time.Millisecond, MaxDelay: time.Minute, Separator: "\n\n"}
	start := time.Now()
	err := received(t, context.Background(), pacing, r, func(emit Emit) error {
		return emit(Text("word"))
	})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("Received took %v, want at least 50ms", elapsed)
	}
	wantTexts(t, r.texts(), []string{"word"})
}

func TestTwoParagraphsInOneChunkAreDeliveredInOrder(t *testing.T) {
	r := newRecorder()
	err := received(t, context.Background(), instant(), r, func(emit Emit) error {
		return emit(Text("first\n\nsecond\n\n"))
	})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	wantTexts(t, r.texts(), []string{"first", "second"})
}

// TestTheFirstParagraphIsDeliveredWhileTheTurnRuns holds the generator until the
// first Reply call has happened, so a delivery that waits for the turn to end
// fails by timeout instead of by a measurement of time.
func TestTheFirstParagraphIsDeliveredWhileTheTurnRuns(t *testing.T) {
	r := newRecorder()
	err := received(t, context.Background(), instant(), r, func(emit Emit) error {
		if err := emit(Text("first\n\n")); err != nil {
			return err
		}
		r.waitForCall(t)
		return emit(Text("second\n\n"))
	})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	wantTexts(t, r.texts(), []string{"first", "second"})
}

func TestTenParagraphsAreAllDeliveredBeforeReceivedReturns(t *testing.T) {
	r := newRecorder()
	want := make([]string, 0, 10)
	var buffer strings.Builder
	for i := range 10 {
		text := string(rune('a' + i))
		want = append(want, text)
		buffer.WriteString(text + "\n\n")
	}
	err := received(t, context.Background(), instant(), r, func(emit Emit) error {
		return emit(Text(buffer.String()))
	})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	wantTexts(t, r.texts(), want)
}

func TestChunksWithoutASeparatorAreOneMessageAtTheEndOfTheTurn(t *testing.T) {
	r := newRecorder()
	err := received(t, context.Background(), instant(), r, func(emit Emit) error {
		for _, text := range []string{"one ", "two ", "three"} {
			if err := emit(Text(text)); err != nil {
				return err
			}
			if len(r.texts()) != 0 {
				t.Errorf("Reply was called before the turn ended: %q", r.texts())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	wantTexts(t, r.texts(), []string{"one two three"})
}

func TestASeparatorAcrossAChunkBoundaryClosesAParagraph(t *testing.T) {
	r := newRecorder()
	err := received(t, context.Background(), instant(), r, func(emit Emit) error {
		for _, text := range []string{"first\n", "\nsecond"} {
			if err := emit(Text(text)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	wantTexts(t, r.texts(), []string{"first", "second"})
}

func TestAConfiguredSeparatorSplitsOnItAlone(t *testing.T) {
	r := newRecorder()
	pacing := Pacing{MaxDelay: time.Minute, Separator: "---"}
	err := received(t, context.Background(), pacing, r, func(emit Emit) error {
		return emit(Text("first\n\nstill first---second"))
	})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	wantTexts(t, r.texts(), []string{"first\n\nstill first", "second"})
}

func TestWhitespaceOnlyContentIsNotDelivered(t *testing.T) {
	r := newRecorder()
	err := received(t, context.Background(), instant(), r, func(emit Emit) error {
		return emit(Text("  \n\n\t"))
	})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	if calls := r.texts(); len(calls) != 0 {
		t.Fatalf("Reply calls = %q, want none", calls)
	}
}

func TestANonTextBlockFlushesThePendingTextFirst(t *testing.T) {
	r := newRecorder()
	image := ContentBlock{Type: "image", Data: "AAAA", MimeType: "image/png"}
	err := received(t, context.Background(), instant(), r, func(emit Emit) error {
		return emit([]ContentBlock{{Type: "text", Text: "look"}, image})
	})
	if err != nil {
		t.Fatalf("Received: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	want := [][]ContentBlock{Text("look"), {image}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("Reply calls = %+v, want %+v", r.calls, want)
	}
}

// TestANonTextBlockGoesOutWithNoDelay runs with a pause long enough to fail the
// test if the image were paced: nothing in the block is words to read.
func TestANonTextBlockGoesOutWithNoDelay(t *testing.T) {
	r := newRecorder()
	pacing := Pacing{DelayPerWord: time.Minute, MaxDelay: time.Hour, Separator: "\n\n"}
	image := ContentBlock{Type: "image", Data: "AAAA", MimeType: "image/png"}
	done := make(chan error, 1)
	go func() {
		done <- received(t, context.Background(), pacing, r, func(emit Emit) error {
			return emit([]ContentBlock{image})
		})
	}()
	r.waitForCall(t)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Received: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Received did not return, want no pause before a non-text block")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !reflect.DeepEqual(r.calls, [][]ContentBlock{{image}}) {
		t.Fatalf("Reply calls = %+v, want the image alone", r.calls)
	}
}

func TestAFailedReplyStopsTheDelivery(t *testing.T) {
	r := newRecorder()
	r.failOn = 1
	var emitErr error
	err := received(t, context.Background(), instant(), r, func(emit Emit) error {
		if err := emit(Text("first\n\n")); err != nil {
			return err
		}
		r.waitForCall(t)
		for range 100 {
			emitErr = emit(Text("second\n\n"))
			if emitErr != nil {
				return emitErr
			}
			time.Sleep(time.Millisecond)
		}
		return errors.New("emit never reported the failed reply")
	})
	if err == nil || !strings.Contains(err.Error(), "the channel refused the message") {
		t.Fatalf("Received = %v, want the error the reply failed with", err)
	}
	if emitErr == nil || !strings.Contains(emitErr.Error(), "the channel refused the message") {
		t.Fatalf("emit = %v, want the error the reply failed with", emitErr)
	}
	wantTexts(t, r.texts(), []string{"first"})
}

func TestACancelledContextStopsTheDelivery(t *testing.T) {
	r := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	err := received(t, ctx, instant(), r, func(emit Emit) error {
		cancel()
		return emit(Text("nothing leaves\n\n"))
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Received = %v, want a cancelled context", err)
	}
	if calls := r.texts(); len(calls) != 0 {
		t.Fatalf("Reply calls = %q, want none", calls)
	}
}

func TestParagraphsSplitsTheBufferAndKeepsTheRemainder(t *testing.T) {
	closed, rest := paragraphs("first\n\nsecond\n\nthird", "\n\n")
	if !reflect.DeepEqual(closed, []string{"first", "second"}) || rest != "third" {
		t.Fatalf("paragraphs = %q, %q, want [first second], third", closed, rest)
	}
}
