package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// generator is an agent whose turn emits what a test tells it to emit.
type generator func(emit Emit) error

func (g generator) Turn(_ context.Context, _ Message, emit Emit) error { return g(emit) }

// recorder is a Reply that records every call, and fails from the call fails
// names onwards.
type recorder struct {
	mu    sync.Mutex
	calls [][]ContentBlock
	fails error
}

func (r *recorder) reply(_ context.Context, content []ContentBlock) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fails != nil {
		return r.fails
	}
	r.calls = append(r.calls, content)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// texts answers the text of every recorded call, so a test states the whole
// delivery as literals.
func (r *recorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for _, content := range r.calls {
		out = append(out, TextOf(content))
	}
	return out
}

// awaitCount waits for the recorder to hold n calls, so a test observes a
// delivery that happened while the turn was still running.
func (r *recorder) awaitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d calls were recorded, want %d", r.count(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// deliver runs one turn of a generator through the delivery under test.
func deliver(ctx context.Context, d Delivery, rec *recorder, g generator) error {
	h := d.Handler(NewHandler(discard(), g))
	return h.Received(ctx, Message{Conversation: "stub:1", Content: Text("hi")}, rec.reply)
}

// emitText is the turn of an agent that emits each string as one text chunk.
func emitText(chunks ...string) generator {
	return func(emit Emit) error {
		for _, chunk := range chunks {
			if err := emit(Text(chunk)); err != nil {
				return err
			}
		}
		return nil
	}
}

// paragraphs is the delivery of a channel that splits on a blank line and paces
// nothing, so a test asserts order without measuring time.
var paragraphs = Delivery{Separator: "\n\n", MaxDelay: 10 * time.Minute}

func want(t *testing.T, rec *recorder, texts ...string) {
	t.Helper()
	got := rec.texts()
	if strings.Join(got, "|") != strings.Join(texts, "|") {
		t.Fatalf("delivered %q, want %q", got, texts)
	}
}

func TestPauseIsTheReadingTimeLeftToWait(t *testing.T) {
	const five = "one two three four five"
	for name, tc := range map[string]struct {
		paragraph string
		delivery  Delivery
		spent     time.Duration
		want      time.Duration
	}{
		"nothing spent":                    {paragraph: five, delivery: Delivery{WordsPerMinute: 60, MaxDelay: 10 * time.Minute}, want: 5 * time.Second},
		"four seconds gone":                {paragraph: five, delivery: Delivery{WordsPerMinute: 60, MaxDelay: 10 * time.Minute}, spent: 4 * time.Second, want: time.Second},
		"more spent than the reading time": {paragraph: five, delivery: Delivery{WordsPerMinute: 60, MaxDelay: 10 * time.Minute}, spent: 6 * time.Second, want: 0},
		"capped":                           {paragraph: strings.TrimSpace(strings.Repeat("word ", 200)), delivery: Delivery{WordsPerMinute: 60, MaxDelay: 10 * time.Second}, want: 10 * time.Second},
		"no pace set":                      {paragraph: five, delivery: Delivery{MaxDelay: 10 * time.Minute}, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := pause(tc.paragraph, tc.delivery, tc.spent); got != tc.want {
				t.Fatalf("pause = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSplitCutsOffTheClosedParagraphsOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		buffer, separator string
		paragraphs        []string
		open              string
	}{
		"no separator configured": {buffer: "a\n\nb", paragraphs: nil, open: "a\n\nb"},
		"one closed paragraph":    {buffer: "a\n\nb", separator: "\n\n", paragraphs: []string{"a"}, open: "b"},
		"two closed paragraphs":   {buffer: "a\n\nb\n\n", separator: "\n\n", paragraphs: []string{"a", "b"}, open: ""},
		"empty paragraph dropped": {buffer: "a\n\n\n\nb", separator: "\n\n", paragraphs: []string{"a"}, open: "b"},
	} {
		t.Run(name, func(t *testing.T) {
			got, open := split(tc.buffer, tc.separator)
			if strings.Join(got, "|") != strings.Join(tc.paragraphs, "|") || open != tc.open {
				t.Fatalf("split = %q, %q, want %q, %q", got, open, tc.paragraphs, tc.open)
			}
		})
	}
}

func TestTheQueueWaitsThePaceBeforeTheParagraph(t *testing.T) {
	rec := &recorder{}
	d := Delivery{Separator: "\n\n", WordsPerMinute: 1200, MaxDelay: 10 * time.Minute}
	start := time.Now()

	if err := deliver(context.Background(), d, rec, emitText("word\n\n")); err != nil {
		t.Fatalf("Received: %v", err)
	}
	if took := time.Since(start); took < 50*time.Millisecond {
		t.Fatalf("the turn took %v, want at least 50ms", took)
	}
	want(t, rec, "word")
}

func TestTwoParagraphsInOneChunkAreDeliveredInOrder(t *testing.T) {
	rec := &recorder{}
	if err := deliver(context.Background(), paragraphs, rec, emitText("first\n\nsecond\n\n")); err != nil {
		t.Fatalf("Received: %v", err)
	}
	want(t, rec, "first", "second")
}

// TestTheFirstParagraphIsDeliveredWhileTheTurnRuns holds the generator until the
// first paragraph has reached the contact, so a delivery that waits for the turn
// to end fails by timeout.
func TestTheFirstParagraphIsDeliveredWhileTheTurnRuns(t *testing.T) {
	rec := &recorder{}
	g := generator(func(emit Emit) error {
		if err := emit(Text("first\n\n")); err != nil {
			return err
		}
		rec.awaitCount(t, 1)
		return emit(Text("second\n\n"))
	})

	if err := deliver(context.Background(), paragraphs, rec, g); err != nil {
		t.Fatalf("Received: %v", err)
	}
	want(t, rec, "first", "second")
}

func TestTenParagraphsAtOnceAreAllDeliveredInOrder(t *testing.T) {
	rec := &recorder{}
	chunk := ""
	expected := make([]string, 0, 10)
	for i := range 10 {
		text := string(rune('a' + i))
		chunk += text + "\n\n"
		expected = append(expected, text)
	}

	if err := deliver(context.Background(), paragraphs, rec, emitText(chunk)); err != nil {
		t.Fatalf("Received: %v", err)
	}
	want(t, rec, expected...)
}

// TestChunksWithNoSeparatorLeaveTheTurnAsOneMessage pins what the separator
// closes: nothing until the turn ends, and nothing inserted between the chunks.
func TestChunksWithNoSeparatorLeaveTheTurnAsOneMessage(t *testing.T) {
	rec := &recorder{}
	g := generator(func(emit Emit) error {
		for _, chunk := range []string{"one ", "two ", "three"} {
			if err := emit(Text(chunk)); err != nil {
				return err
			}
		}
		if rec.count() != 0 {
			t.Errorf("%d calls were made before the turn ended, want 0", rec.count())
		}
		return nil
	})

	if err := deliver(context.Background(), paragraphs, rec, g); err != nil {
		t.Fatalf("Received: %v", err)
	}
	want(t, rec, "one two three")
}

func TestASeparatorAcrossAChunkBoundaryClosesAParagraph(t *testing.T) {
	rec := &recorder{}
	if err := deliver(context.Background(), paragraphs, rec, emitText("first\n", "\nsecond")); err != nil {
		t.Fatalf("Received: %v", err)
	}
	want(t, rec, "first", "second")
}

func TestAConfiguredSeparatorIsTheOnlyParagraphEnd(t *testing.T) {
	rec := &recorder{}
	d := Delivery{Separator: "---", MaxDelay: 10 * time.Minute}
	if err := deliver(context.Background(), d, rec, emitText("first\n\nstill first---second")); err != nil {
		t.Fatalf("Received: %v", err)
	}
	want(t, rec, "first\n\nstill first", "second")
}

func TestTheDefaultDeliverySendsTheWholeReplyAsOneMessage(t *testing.T) {
	rec := &recorder{}
	d, err := NewDelivery(func(any) error { return nil })
	if err != nil {
		t.Fatalf("NewDelivery: %v", err)
	}
	g := generator(func(emit Emit) error {
		if err := emit(Text("first\n\nsecond\n\nthird")); err != nil {
			return err
		}
		if rec.count() != 0 {
			t.Errorf("%d calls were made before the turn ended, want 0", rec.count())
		}
		return nil
	})

	if err := deliver(context.Background(), d, rec, g); err != nil {
		t.Fatalf("Received: %v", err)
	}
	want(t, rec, "first\n\nsecond\n\nthird")
}

func TestWhitespaceIsNotDelivered(t *testing.T) {
	rec := &recorder{}
	if err := deliver(context.Background(), paragraphs, rec, emitText("  \n\n\t")); err != nil {
		t.Fatalf("Received: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("delivered %q, want no call", rec.texts())
	}
}

func TestAnImageIsDeliveredAfterThePendingTextAndWithNoPause(t *testing.T) {
	rec := &recorder{}
	d := Delivery{Separator: "\n\n", WordsPerMinute: 120, MaxDelay: 10 * time.Minute}
	image := []ContentBlock{{Type: "image", Data: "AAAA", MimeType: "image/png"}}
	start := time.Now()

	if err := deliver(context.Background(), d, rec, func(emit Emit) error {
		if err := emit(Text("look at this")); err != nil {
			return err
		}
		return emit(image)
	}); err != nil {
		t.Fatalf("Received: %v", err)
	}
	// The text is three words at two words per second, and the image adds nothing.
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the turn took %v: the image waited for a pace of its own", took)
	}
	got := rec.texts()
	if len(got) != 2 || got[0] != "look at this" || got[1] != "" {
		t.Fatalf("delivered %q, want the text then the image", got)
	}
	if rec.calls[1][0].Type != "image" {
		t.Fatalf("second call carried %+v, want the image block", rec.calls[1])
	}
}

func TestAFailedReplyStopsTheDelivery(t *testing.T) {
	broken := errors.New("the contact is gone")
	rec := &recorder{fails: broken}
	var emitted error
	g := generator(func(emit Emit) error {
		if err := emit(Text("first\n\n")); err != nil {
			return err
		}
		for {
			emitted = emit(Text("second\n\n"))
			if emitted != nil {
				return emitted
			}
			time.Sleep(time.Millisecond)
		}
	})

	err := deliver(context.Background(), paragraphs, rec, g)
	if !errors.Is(err, broken) {
		t.Fatalf("Received = %v, want it to wrap %v", err, broken)
	}
	if !errors.Is(emitted, broken) {
		t.Fatalf("emit = %v, want it to wrap %v", emitted, broken)
	}
	if rec.count() != 0 {
		t.Fatalf("%d calls succeeded, want none after the failure", rec.count())
	}
}

// TestACancelledTurnStillDeliversWhatItQueued pins that everything the agent
// generated reaches the contact: the session history of the agent and what the
// contact received stay the same.
func TestACancelledTurnStillDeliversWhatItQueued(t *testing.T) {
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	d := Delivery{Separator: "\n\n", WordsPerMinute: 1200, MaxDelay: 10 * time.Minute}
	g := generator(func(emit Emit) error {
		if err := emit(Text("a\n\nb\n\n")); err != nil {
			return err
		}
		cancel()
		return emit(Text("c\n\nd\n\n"))
	})

	if err := deliver(ctx, d, rec, g); err != nil {
		t.Fatalf("Received: %v", err)
	}
	want(t, rec, "a", "b", "c", "d")
}

func TestTheQueueSendsWithAContextThatIsNotCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var sent []error
	reply := func(ctx context.Context, _ []ContentBlock) error {
		sent = append(sent, ctx.Err())
		return nil
	}
	h := paragraphs.Handler(NewHandler(discard(), generator(func(emit Emit) error {
		cancel()
		return emit(Text("one\n\n"))
	})))

	if err := h.Received(ctx, Message{Conversation: "stub:1", Content: Text("hi")}, reply); err != nil {
		t.Fatalf("Received: %v", err)
	}
	if len(sent) != 1 || sent[0] != nil {
		t.Fatalf("Reply saw %v, want one call with a context that is not cancelled", sent)
	}
}

func TestNewDeliveryRefusesUnusableSettings(t *testing.T) {
	for name, s := range map[string]deliverySettings{
		"negative words_per_minute": {WordsPerMinute: -1},
		"max_delay of zero":         {MaxDelay: new(time.Duration)},
	} {
		t.Run(name, func(t *testing.T) {
			settings := s
			if _, err := NewDelivery(func(v any) error {
				*(v.(*deliverySettings)) = settings
				return nil
			}); err == nil {
				t.Fatal("NewDelivery succeeded, want an error naming the setting")
			}
		})
	}
}

func TestNewDeliveryDefaults(t *testing.T) {
	d, err := NewDelivery(func(any) error { return nil })
	if err != nil {
		t.Fatalf("NewDelivery: %v", err)
	}
	if d.Separator != "" || d.WordsPerMinute != 0 || d.MaxDelay != 10*time.Minute {
		t.Fatalf("delivery = %+v, want no separator, no pace and a 10m cap", d)
	}
}
