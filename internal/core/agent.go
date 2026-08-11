package core

import (
	"context"
	"log/slog"
	"sync"
)

// Agent runs a conversation's turns: the prompt goes in and the whole reply
// comes back. How an agent is reached, and what it costs to reach it, is its
// implementation's business; the core only knows a turn belongs to one
// conversation.
type Agent interface {
	Turn(ctx context.Context, conversation string, prompt []ContentBlock) ([]ContentBlock, error)
}

// AgentHandler answers an incoming message with the agent's reply, one turn at a
// time per conversation. Unrelated conversations run at the same time.
type AgentHandler struct {
	Agent Agent
	Log   *slog.Logger

	// ponytail: a mutex per conversation, kept forever and granted in whatever
	// order the runtime picks. The per-contact actor with its own FIFO inbox
	// replaces it, and takes steering and harness-death retries with it.
	turns sync.Map
}

func (h *AgentHandler) Received(ctx context.Context, m Message, reply Reply) {
	h.Log.Info("message received", attrs(m)...)
	turn := h.turnLock(m.Contact)
	turn.Lock()
	defer turn.Unlock()

	content, err := h.Agent.Turn(ctx, m.Contact, m.Content)
	if err != nil {
		h.Log.Error("turn failed", "contact", m.Contact, "error", err)
		return
	}
	if err := reply(ctx, content); err != nil {
		h.Log.Error("reply failed", "contact", m.Contact, "error", err)
	}
}

func (h *AgentHandler) Sent(_ context.Context, m Message) {
	h.Log.Info("message sent", attrs(m)...)
}

func (h *AgentHandler) turnLock(conversation string) *sync.Mutex {
	lock, _ := h.turns.LoadOrStore(conversation, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// attrs reports a message's block types and its text, and never the base64 data
// an image, audio or blob block carries: one of those would write megabytes into
// a single log record.
func attrs(m Message) []any {
	types := make([]string, 0, len(m.Content))
	for _, block := range m.Content {
		types = append(types, block.Type)
	}
	return []any{"contact", m.Contact, "blocks", types, "text", TextOf(m.Content)}
}
