package core

import (
	"context"
	"log/slog"
	"slices"
	"sync"
)

// Serialize runs one turn of a conversation at a time, delivery included, and
// sends the messages that arrive meanwhile as the prompt of the next turn. The
// answer is the route a channel answers a contact on.
//
// records is the route that writes a message into a conversation without
// answering it. It carries no prompt of the contact, so nothing is merged into
// it: it waits for the conversation to be free instead, and a message that
// arrives meanwhile waits for it. Both routes share one conversation, so a record
// and a turn of the same conversation never run at the same time, and both reach
// the agent in the order their calls entered. A nil records is answered with nil,
// so a caller that needs the route can tell it is absent.
func Serialize(log *slog.Logger, turns, records Handler) (Handler, Handler) {
	c := &conversations{log: log, handler: turns, state: map[string]*conversation{}}
	if records == nil {
		return c, nil
	}
	return c, recording{conversations: c, handler: records}
}

type conversations struct {
	log     *slog.Logger
	handler Handler

	mu sync.Mutex
	// state holds the conversations that are busy, and nothing else: an entry is
	// created when a turn starts and removed when the conversation goes quiet.
	state map[string]*conversation
}

type conversation struct {
	pending *batch
	// batching says that a turn holds the conversation, and that a message which
	// arrives now becomes the prompt of the next turn. A record holds it without
	// batching, because its prompt is one message and one command.
	batching bool
	// queue holds the callers that wait, in the order their calls entered. Whoever
	// holds the conversation hands it to the first of them, so nothing overtakes a
	// call that entered earlier. A pending batch entered before every queued caller,
	// because a message joins it only while the queue is empty.
	queue []*ticket
}

// ticket is the place of one waiting caller. granted says under the lock that the
// conversation was handed to it, so a caller that gave up can tell whether it
// still has to hand the conversation on.
type ticket struct {
	ready    chan struct{}
	batching bool
	granted  bool
}

// batch is the content of the messages that arrived during a turn, and the
// outcome of the turn that carries them.
type batch struct {
	content []ContentBlock
	// done is closed once err holds the outcome of the batch's turn.
	done chan struct{}
	err  error
}

func (c *conversations) Received(ctx context.Context, m Message, reply Reply) error {
	b, t := c.join(m)
	if b != nil {
		c.log.Info("message joined the pending batch", attrs(m)...)
		select {
		case <-b.done:
			return b.err
		case <-ctx.Done():
			// The content stays in the batch: a message momo accepted is not dropped
			// because the transport that brought it gave up.
			return ctx.Err()
		}
	}
	if t != nil {
		if err := c.await(ctx, m.Conversation, t); err != nil {
			return err
		}
	}
	return c.run(ctx, m, reply)
}

func (c *conversations) Sent(ctx context.Context, m Message) { c.handler.Sent(ctx, m) }

// join appends the message to the batch of a conversation a turn holds, and
// answers that batch. It answers nothing when the conversation was free, and the
// caller then owns its turns. It answers a ticket when the message cannot join,
// and the caller then waits for its place in the queue.
func (c *conversations) join(m Message) (*batch, *ticket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, busy := c.state[m.Conversation]
	if busy && state.batching && len(state.queue) == 0 {
		if state.pending == nil {
			state.pending = &batch{done: make(chan struct{})}
		}
		state.pending.content = append(state.pending.content, m.Content...)
		return state.pending, nil
	}
	return nil, c.enter(m.Conversation, true)
}

// enter takes a free conversation and answers nothing, or queues the caller behind
// whoever holds it. The caller holds the lock.
func (c *conversations) enter(id string, batching bool) *ticket {
	state, busy := c.state[id]
	if !busy {
		c.state[id] = &conversation{batching: batching}
		return nil
	}
	t := &ticket{ready: make(chan struct{}), batching: batching}
	state.queue = append(state.queue, t)
	return t
}

func (c *conversations) await(ctx context.Context, id string, t *ticket) error {
	select {
	case <-t.ready:
		return nil
	case <-ctx.Done():
		c.abandon(id, t)
		return ctx.Err()
	}
}

// abandon gives up the place of a caller that stopped waiting. A conversation the
// caller was already given is passed on, so the queue behind it still runs.
func (c *conversations) abandon(id string, t *ticket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state[id]
	if t.granted {
		c.pass(id, state)
		return
	}
	if i := slices.Index(state.queue, t); i >= 0 {
		state.queue = slices.Delete(state.queue, i, i+1)
	}
}

// run runs the turn of this message and then a turn for each batch that arrived
// behind it, on the caller's goroutine: the caller is inside Received and is
// therefore able to deliver, while a waiting caller may have given up.
func (c *conversations) run(ctx context.Context, m Message, reply Reply) error {
	err := c.handler.Received(ctx, m, reply)
	for {
		b := c.next(m.Conversation)
		if b == nil {
			return err
		}
		b.err = c.handler.Received(ctx, Message{Conversation: m.Conversation, Content: b.content}, reply)
		close(b.done)
	}
}

// next takes the pending batch of the conversation. When there is none, it passes
// the conversation on and answers nothing.
func (c *conversations) next(conversation string) *batch {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state[conversation]
	if state.pending != nil {
		b := state.pending
		state.pending = nil
		return b
	}
	c.pass(conversation, state)
	return nil
}

// pass hands the conversation to the caller that entered first, and forgets a
// conversation nobody waits for. The caller holds the lock.
func (c *conversations) pass(id string, state *conversation) {
	if len(state.queue) == 0 {
		delete(c.state, id)
		return
	}
	t := state.queue[0]
	state.queue = state.queue[1:]
	state.batching = t.batching
	t.granted = true
	close(t.ready)
}

// take holds the conversation exclusively, behind whoever entered earlier.
func (c *conversations) take(ctx context.Context, id string) error {
	c.mu.Lock()
	t := c.enter(id, false)
	c.mu.Unlock()
	if t == nil {
		return nil
	}
	return c.await(ctx, id, t)
}

// release frees a conversation a record held. Nothing joins a record, so there is
// no batch to run first.
func (c *conversations) release(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pass(id, c.state[id])
}

// recording is the route of a message that is written into a conversation instead
// of answered. It holds the conversation for the time of the record, so the
// agent's session keeps the order of what was said.
type recording struct {
	conversations *conversations
	handler       Handler
}

func (r recording) Received(ctx context.Context, m Message, reply Reply) error {
	if err := r.conversations.take(ctx, m.Conversation); err != nil {
		return err
	}
	defer r.conversations.release(m.Conversation)
	return r.handler.Received(ctx, m, reply)
}

func (r recording) Sent(ctx context.Context, m Message) {
	if err := r.conversations.take(ctx, m.Conversation); err != nil {
		r.conversations.log.Error("outgoing message not recorded", "conversation", m.Conversation, "error", err)
		return
	}
	defer r.conversations.release(m.Conversation)
	r.handler.Sent(ctx, m)
}
