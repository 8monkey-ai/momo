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
	// Conversation names the conversation the message belongs to, qualified by the
	// channel it arrived on: two channels using the same contact id are two
	// conversations. A channel fills in its own contact id and the qualification
	// is added for it, so it cannot be forgotten or misreported.
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

// Agent runs one turn of a conversation: the message goes in as a prompt and the
// agent's whole reply comes back. What an agent is, where it runs and what it
// speaks belong to the implementation, so a turn is expressible here without any
// of it.
type Agent interface {
	Turn(ctx context.Context, conversation string, prompt []ContentBlock) ([]ContentBlock, error)
}

// AgentHandler answers an incoming message with the agent's reply for the
// conversation it belongs to.
type AgentHandler struct {
	Agent Agent
	Log   *slog.Logger
}

func (h AgentHandler) Received(ctx context.Context, m Message, reply Reply) {
	h.Log.Info("message received", attrs(m)...)
	content, err := h.Agent.Turn(ctx, m.Conversation, m.Content)
	if err != nil {
		h.Log.Error("turn failed", "conversation", m.Conversation, "error", err)
		return
	}
	if err := reply(ctx, content); err != nil {
		h.Log.Error("reply failed", "conversation", m.Conversation, "error", err)
	}
}

func (h AgentHandler) Sent(_ context.Context, m Message) {
	h.Log.Info("message sent", attrs(m)...)
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
