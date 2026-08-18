// Package core holds momo's view of a conversation: the messages channels
// deliver and the action each direction leads to.
package core

import (
	"context"
	"log/slog"
	"strings"
)

// ContentBlock is one block of a message's content, in ACP v1's shape: a
// message arriving as ACP reaches the core unchanged and is forwardable to an
// agent harness as it was sent. Only the fields v1 requires are modelled.
// Channels that do not speak ACP convert at their own edge.
type ContentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	Data     string    `json:"data,omitempty"`
	MimeType string    `json:"mimeType,omitempty"`
	URI      string    `json:"uri,omitempty"`
	Name     string    `json:"name,omitempty"`
	Resource *Resource `json:"resource,omitempty"`
}

// Resource is the contents an embedded resource block carries.
type Resource struct {
	URI  string `json:"uri"`
	Text string `json:"text,omitempty"`
	Blob string `json:"blob,omitempty"`
}

// Text is a text content block, the shape a channel with plain text messages
// converts into.
func Text(s string) []ContentBlock { return []ContentBlock{{Type: "text", Text: s}} }

// TextOf is the inverse of Text: the text blocks carry, joined, and nothing of
// the base64 payload an image, audio or blob block holds. A channel that speaks
// plain text converts with it at its own edge.
func TextOf(content []ContentBlock) string {
	var text []string
	for _, block := range content {
		if block.Text != "" {
			text = append(text, block.Text)
		}
	}
	return strings.Join(text, " ")
}

// Message is a message exchanged with a contact, in the shape the core acts on.
type Message struct {
	// Conversation is the conversation the message belongs to. A channel fills it
	// with its own contact id, and channel.Build qualifies it with the channel
	// name, so a handler always reads {channel}:{contact}.
	Conversation string
	Content      []ContentBlock
}

// Reply sends a reply on the channel the message arrived on. The destination is
// fixed when the message arrives, so it is not a parameter. It is safe to call
// from any goroutine.
type Reply func(ctx context.Context, content []ContentBlock) error

// Handler is what a channel delivers messages to. The two directions are
// separate methods because each is an occasion for a different action, and only
// the incoming one has something to answer.
type Handler interface {
	Received(ctx context.Context, m Message, reply Reply)
	Sent(ctx context.Context, m Message)
}

// Agent runs one turn of one conversation: the message goes in, and the complete
// reply comes out. The implementation is outside the core, so the core carries no
// protocol and no process.
type Agent interface {
	Turn(ctx context.Context, m Message) ([]ContentBlock, error)
}

// NewHandler answers each incoming message with the reply of one agent turn, on
// the channel the message arrived on.
func NewHandler(log *slog.Logger, a Agent) Handler {
	return handler{log: log, agent: a}
}

type handler struct {
	log   *slog.Logger
	agent Agent
}

func (h handler) Received(ctx context.Context, m Message, reply Reply) {
	h.log.Info("message received", attrs(m)...)
	content, err := h.agent.Turn(ctx, m)
	if err != nil {
		h.log.Error("turn failed", "conversation", m.Conversation, "error", err)
		return
	}
	if err := reply(ctx, content); err != nil {
		h.log.Error("reply failed", "conversation", m.Conversation, "error", err)
	}
}

func (h handler) Sent(_ context.Context, m Message) {
	h.log.Info("message sent", attrs(m)...)
}

// attrs reports a message's block types and its text, and never the base64 data
// an image, audio or blob block carries: one of those would write megabytes into
// a single log record.
func attrs(m Message) []any {
	types := make([]string, 0, len(m.Content))
	for _, block := range m.Content {
		types = append(types, block.Type)
	}
	return []any{"conversation", m.Conversation, "blocks", types, "text", TextOf(m.Content)}
}
