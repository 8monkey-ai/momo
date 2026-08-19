package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Delivery is how a channel puts a turn's reply in front of a contact: where a
// paragraph ends, and how long the contact waits for it. It belongs to the
// channel, because a contact reading on a phone and a program reading a stream
// want different paces.
type Delivery struct {
	// Separator closes a paragraph. Empty keeps the whole reply in one message.
	Separator string
	// WordsPerMinute paces the pause before a paragraph. Zero pauses for nothing.
	WordsPerMinute int
	// MaxDelay caps the pause of one paragraph.
	MaxDelay time.Duration
}

type deliverySettings struct {
	Separator      string `yaml:"separator"`
	WordsPerMinute int    `yaml:"words_per_minute"`
	// A pointer tells an absent setting, which takes the default, from a value the
	// operator set, which is refused when it caps a pause at nothing.
	MaxDelay *time.Duration `yaml:"max_delay"`
}

// NewDelivery reads a channel's delivery block and refuses settings momo cannot
// deliver with, before the process serves anything.
func NewDelivery(decode func(any) error) (Delivery, error) {
	var s deliverySettings
	if err := decode(&s); err != nil {
		return Delivery{}, err
	}
	if s.WordsPerMinute < 0 {
		return Delivery{}, errors.New("words_per_minute cannot be negative")
	}
	maxDelay := 10 * time.Minute
	if s.MaxDelay != nil {
		if *s.MaxDelay <= 0 {
			return Delivery{}, errors.New("max_delay must be positive")
		}
		maxDelay = *s.MaxDelay
	}
	return Delivery{Separator: s.Separator, WordsPerMinute: s.WordsPerMinute, MaxDelay: maxDelay}, nil
}

// Handler delivers what h replies under these settings: the reply is split into
// paragraphs and each one is sent as its own message, paced, while the turn is
// still generating the rest.
func (d Delivery) Handler(h Handler) Handler { return delivering{delivery: d, handler: h} }

type delivering struct {
	delivery Delivery
	handler  Handler
}

func (d delivering) Received(ctx context.Context, m Message, reply Reply) error {
	q := start(ctx, d.delivery, reply)
	err := d.handler.Received(ctx, m, q.accept)
	// The queue is drained in every case, so nothing outlives the turn, and a
	// delivery that failed after the turn succeeded is still reported.
	if failed := q.drain(); err == nil {
		err = failed
	}
	return err
}

func (d delivering) Sent(ctx context.Context, m Message) { d.handler.Sent(ctx, m) }

// queue delivers one turn's parts in order, on a goroutine of its own. Ordering
// comes from that single consumer. A paragraph's pause does not block the agent,
// which is still generating the rest of the reply.
type queue struct {
	// send outlives the turn's context, so a turn that was stopped still delivers
	// what it generated instead of failing every remaining send.
	send     context.Context
	delivery Delivery
	reply    Reply
	// signal wakes the consumer. Capacity one is enough: it says that the queue
	// changed, not how.
	signal chan struct{}
	done   chan struct{}

	mu sync.Mutex
	// open is the text no separator has closed yet. A chunk arrives on the goroutine
	// the agent dispatches on, so it is held under the lock like the rest.
	open   string
	parts  [][]ContentBlock
	closed bool
	err    error
}

func start(ctx context.Context, d Delivery, reply Reply) *queue {
	q := &queue{
		send:     context.WithoutCancel(ctx),
		delivery: d,
		reply:    reply,
		signal:   make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	// The first paragraph is paced from the start of the turn.
	go q.run(time.Now())
	return q
}

// accept takes one part of the reply as the agent produces it, and answers what
// an earlier part failed with. It never blocks: the agent dispatches its messages
// serially, so an agent held here would stop answering the permission request of
// its own turn.
func (q *queue) accept(_ context.Context, content []ContentBlock) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, block := range content {
		if block.Type == "text" {
			closed, open := split(q.open+block.Text, q.delivery.Separator)
			q.open = open
			for _, paragraph := range closed {
				q.add(Text(paragraph))
			}
			continue
		}
		// A block momo cannot split is a message of its own, and the text written
		// before it belongs in front of it.
		q.closeOpen()
		q.add([]ContentBlock{block})
	}
	return q.err
}

// drain closes the text the turn left open and waits for the queue to empty, so
// the caller learns of the last paragraph before it answers its own transport.
func (q *queue) drain() error {
	q.mu.Lock()
	q.closeOpen()
	q.closed = true
	q.mu.Unlock()
	q.wake()
	<-q.done
	return q.failure()
}

// closeOpen queues the text no separator closed. The caller holds the lock.
func (q *queue) closeOpen() {
	paragraph := strings.TrimSpace(q.open)
	q.open = ""
	if paragraph == "" {
		return
	}
	q.add(Text(paragraph))
}

// add queues one part and wakes the consumer. The caller holds the lock.
func (q *queue) add(content []ContentBlock) {
	q.parts = append(q.parts, content)
	q.wake()
}

func (q *queue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *queue) run(last time.Time) {
	defer close(q.done)
	for {
		content, pending := q.next()
		if pending {
			<-q.signal
			continue
		}
		if content == nil {
			return
		}
		time.Sleep(pause(TextOf(content), q.delivery, time.Since(last)))
		if err := q.reply(q.send, content); err != nil {
			q.fail(err)
			return
		}
		last = time.Now()
	}
}

// next answers the part at the head of the queue. pending is true while the queue
// is empty and the turn may still add to it.
func (q *queue) next() (content []ContentBlock, pending bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return nil, false
	}
	if len(q.parts) == 0 {
		return nil, !q.closed
	}
	content, q.parts = q.parts[0], q.parts[1:]
	return content, false
}

// fail ends the turn's delivery: what is queued is dropped, and every later
// accept answers with this error. Only a failed Reply ends a delivery, because
// the transport that would carry the rest is the one that just failed.
func (q *queue) fail(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err == nil {
		q.err = err
	}
	q.parts = nil
}

func (q *queue) failure() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.err
}

// split cuts off every paragraph the separator closes and answers the text that
// is still open. Whitespace around a paragraph is dropped, and a paragraph that
// holds nothing else is dropped with it.
func split(buffer, separator string) (closed []string, open string) {
	if separator == "" {
		return nil, buffer
	}
	parts := strings.Split(buffer, separator)
	for _, part := range parts[:len(parts)-1] {
		if paragraph := strings.TrimSpace(part); paragraph != "" {
			closed = append(closed, paragraph)
		}
	}
	return closed, parts[len(parts)-1]
}

// pause is how long a paragraph waits before it is sent: its reading time at the
// configured pace, capped, less the time the turn already spent producing it.
func pause(paragraph string, d Delivery, spent time.Duration) time.Duration {
	if d.WordsPerMinute <= 0 {
		return 0
	}
	reading := time.Duration(len(strings.Fields(paragraph))) * time.Minute / time.Duration(d.WordsPerMinute)
	return max(min(reading, d.MaxDelay)-spent, 0)
}
