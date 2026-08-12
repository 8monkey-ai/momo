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
	Contact string
	Content []ContentBlock
}

// Reply sends a reply on the channel the message arrived on. The destination is
// fixed when the message arrives, so it is not a parameter. It is safe to call
// from any goroutine.
type Reply func(ctx context.Context, content []ContentBlock) error

// Agent runs one turn of one conversation: the incoming message goes in, the
// complete reply comes out. The implementation lives outside core, so core knows
// neither ACP nor that a subprocess exists.
type Agent interface {
	Turn(ctx context.Context, conversation string, prompt []ContentBlock) ([]ContentBlock, error)
}

// Handler is what a channel delivers messages to. The two directions are
// separate methods because each is an occasion for a different action, and only
// the incoming one has something to answer. Received reports a turn it could not
// complete, so the channel that received the message can tell the sender in the
// way that channel has.
type Handler interface {
	Received(ctx context.Context, m Message, reply Reply) error
	Sent(ctx context.Context, m Message)
}

// AgentHandler answers every incoming message with one turn of the agent.
type AgentHandler struct {
	Agent Agent
	Log   *slog.Logger
}

// Received runs one turn and sends the complete reply once, before the turn is
// reported complete.
func (h AgentHandler) Received(ctx context.Context, m Message, reply Reply) error {
	h.Log.Info("message received", attrs(m)...)
	content, err := h.Agent.Turn(ctx, m.Contact, m.Content)
	if err != nil {
		h.Log.Error("turn failed", "contact", m.Contact, "error", err)
		return err
	}
	if err := reply(ctx, content); err != nil {
		h.Log.Error("reply failed", "contact", m.Contact, "error", err)
		return err
	}
	return nil
}

func (h AgentHandler) Sent(_ context.Context, m Message) {
	h.Log.Info("message sent", attrs(m)...)
}

// Qualify names every message a channel delivers with the channel it arrived on,
// as {channel}:{contact}. Two channels can issue the same contact id, so the
// channel name is what makes a conversation identity unique. Every handler is
// wrapped with it where the channels are built, so no channel can forget it or
// state it wrongly.
func Qualify(channel string, h Handler) Handler {
	return qualified{channel: channel, handler: h}
}

type qualified struct {
	channel string
	handler Handler
}

func (q qualified) Received(ctx context.Context, m Message, reply Reply) error {
	return q.handler.Received(ctx, q.name(m), reply)
}

func (q qualified) Sent(ctx context.Context, m Message) { q.handler.Sent(ctx, q.name(m)) }

func (q qualified) name(m Message) Message {
	m.Contact = q.channel + ":" + m.Contact
	return m
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
