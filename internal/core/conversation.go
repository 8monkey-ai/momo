package core

import (
	"context"
	"log/slog"
	"sync"
)

// Serialize runs one turn of a conversation at a time, delivery included, and
// sends the messages that arrive meanwhile as the prompt of the next turn.
func Serialize(log *slog.Logger, h Handler) Handler {
	return &conversations{log: log, handler: h, state: map[string]*conversation{}}
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
	if b := c.join(m); b != nil {
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
	return c.run(ctx, m, reply)
}

func (c *conversations) Sent(ctx context.Context, m Message) { c.handler.Sent(ctx, m) }

// join appends the message to the batch of a busy conversation, and answers that
// batch. It answers nil when the conversation was free, and the caller then owns
// its turns.
func (c *conversations) join(m Message) *batch {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, busy := c.state[m.Conversation]
	if !busy {
		c.state[m.Conversation] = &conversation{}
		return nil
	}
	if state.pending == nil {
		state.pending = &batch{done: make(chan struct{})}
	}
	state.pending.content = append(state.pending.content, m.Content...)
	return state.pending
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

// next takes the pending batch of the conversation, and frees the conversation
// when there is none.
func (c *conversations) next(conversation string) *batch {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.state[conversation]
	if state.pending == nil {
		delete(c.state, conversation)
		return nil
	}
	b := state.pending
	state.pending = nil
	return b
}
