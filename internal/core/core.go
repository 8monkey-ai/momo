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
// Contact names the conversation the message belongs to, qualified with the
// channel that received it, because two channels can issue the same contact id.
// Qualify does that, and a handler never sees an unqualified message.
type Message struct {
	Contact string
	Content []ContentBlock
}

// Qualify names the conversation a message belongs to: the contact id of the
// channel, under the name that channel was configured with.
func Qualify(channel string, m Message) Message {
	m.Contact = channel + ":" + m.Contact
	return m
}

// Reply sends a reply on the channel the message arrived on. The destination is
// fixed when the message arrives, so it is not a parameter. It is safe to call
// from any goroutine.
type Reply func(ctx context.Context, content []ContentBlock) error

// Handler is what a channel delivers messages to. The two directions are
// separate methods because each is an occasion for a different action, and only
// the incoming one has something to answer.
//
// Received reports a turn that produced no reply, so the channel that received
// the message can report the failure the way that channel reports one.
type Handler interface {
	Received(ctx context.Context, m Message, reply Reply) error
	Sent(ctx context.Context, m Message)
}

// Agent runs one turn of one conversation: the prompt goes in, and the reply
// leaves through reply before the turn is complete. An error is a turn that
// produced no reply.
type Agent interface {
	Turn(ctx context.Context, conversation string, prompt []ContentBlock, reply Reply) error
}

// Turn is the handler that answers an incoming message with one agent turn.
type Turn struct {
	Agent Agent
	Log   *slog.Logger
}

func (t Turn) Received(ctx context.Context, m Message, reply Reply) error {
	t.Log.Info("message received", attrs(m)...)
	if err := t.Agent.Turn(ctx, m.Contact, m.Content, reply); err != nil {
		t.Log.Error("turn failed", "conversation", m.Contact, "error", err)
		return err
	}
	return nil
}

func (t Turn) Sent(_ context.Context, m Message) {
	t.Log.Info("message sent", attrs(m)...)
}

// attrs reports a message's block types and its text, and never the base64 data
// an image, audio or blob block carries: one of those would write megabytes into
// a single log record.
func attrs(m Message) []any {
	types := make([]string, 0, len(m.Content))
	for _, block := range m.Content {
		types = append(types, block.Type)
	}
	return []any{"conversation", m.Contact, "blocks", types, "text", TextOf(m.Content)}
}
