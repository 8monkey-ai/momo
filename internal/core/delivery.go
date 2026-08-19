package core

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Pacing is how a turn's reply reaches the contact: the separator that closes a
// paragraph, and the pause that comes before one.
type Pacing struct {
	DelayPerWord time.Duration
	MaxDelay     time.Duration
	Separator    string
}

// pause is how long the contact waits before a paragraph arrives: one contact
// reads at a pace, and a reply that lands complete and at once reads as a
// machine's.
func pause(text string, p Pacing) time.Duration {
	return min(time.Duration(len(strings.Fields(text)))*p.DelayPerWord, p.MaxDelay)
}

// paragraphs answers the paragraphs the buffer closed and the text after the
// last separator, which the turn may still add to.
func paragraphs(buffer, separator string) (closed []string, rest string) {
	parts := strings.Split(buffer, separator)
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// part is one message the delivery is ready to send, and the pause before it.
type part struct {
	content []ContentBlock
	pause   time.Duration
}

// delivery sends the parts of one turn's reply, in order, on a goroutine of its
// own. The agent hands it content with emit, which never blocks: a backlog grows
// in the queue instead, so an agent whose connection also carries a permission
// request is never held up by a pause. close ends the turn and waits for the
// queue to drain.
//
// ponytail: one pacing for every channel. Give Pacing a per-channel origin if a
// peer that wants no pause, such as an ACP client, has to be told apart from a
// contact reading on a phone.
type delivery struct {
	ctx    context.Context
	pacing Pacing
	reply  Reply

	mu     sync.Mutex
	buffer string
	queue  []part
	closed bool
	err    error

	ready chan struct{}
	done  chan struct{}
}

func newDelivery(ctx context.Context, pacing Pacing, reply Reply) *delivery {
	d := &delivery{
		ctx:    ctx,
		pacing: pacing,
		reply:  reply,
		ready:  make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	go d.run()
	return d
}

func (d *delivery) emit(content []ContentBlock) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	for _, block := range content {
		if block.Type == "text" {
			d.buffer += block.Text
			var closed []string
			closed, d.buffer = paragraphs(d.buffer, d.pacing.Separator)
			for _, paragraph := range closed {
				d.enqueue(paragraph)
			}
			continue
		}
		// A block momo cannot read belongs after the text that introduces it, and
		// carries no words to pace it with.
		d.flush()
		d.queue = append(d.queue, part{content: []ContentBlock{block}})
	}
	d.wake()
	return nil
}

// close delivers what the turn left pending and answers with the error the
// delivery ended on, if any.
func (d *delivery) close() error {
	d.mu.Lock()
	d.flush()
	d.closed = true
	d.wake()
	d.mu.Unlock()
	<-d.done
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// flush closes the pending text. The caller holds the mutex.
func (d *delivery) flush() {
	text := d.buffer
	d.buffer = ""
	d.enqueue(text)
}

// enqueue drops a paragraph that carries no text: a separator at the end of a
// chunk, or a turn that ended on one, closes an empty paragraph, and an empty
// message is nothing to send. The caller holds the mutex.
func (d *delivery) enqueue(text string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	d.queue = append(d.queue, part{content: Text(trimmed), pause: pause(trimmed, d.pacing)})
}

// wake tells the goroutine that the queue changed, without blocking: the channel
// holds one signal, and one is enough to make the consumer look again.
func (d *delivery) wake() {
	select {
	case d.ready <- struct{}{}:
	default:
	}
}

func (d *delivery) run() {
	defer close(d.done)
	for {
		next, ready, drained := d.take()
		if drained {
			return
		}
		if !ready {
			select {
			case <-d.ready:
			case <-d.ctx.Done():
				d.fail(d.ctx.Err())
				return
			}
			continue
		}
		if err := d.wait(next.pause); err != nil {
			d.fail(err)
			return
		}
		if err := d.reply(d.ctx, next.content); err != nil {
			d.fail(err)
			return
		}
	}
}

// wait holds the pause. A context already cancelled ends the delivery even when
// the pause is zero, so nothing is sent after cancellation.
func (d *delivery) wait(p time.Duration) error {
	if err := d.ctx.Err(); err != nil {
		return err
	}
	select {
	case <-time.After(p):
		return nil
	case <-d.ctx.Done():
		return d.ctx.Err()
	}
}

// take answers the next part, whether one was ready, and whether the turn is
// over and the queue empty.
func (d *delivery) take() (next part, ready, drained bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) > 0 {
		next, d.queue = d.queue[0], d.queue[1:]
		return next, true, false
	}
	return part{}, false, d.closed
}

// fail records the reason the delivery ended, so the pending parts are dropped
// and the next emit tells the agent to stop generating.
func (d *delivery) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err == nil {
		d.err = err
	}
}
