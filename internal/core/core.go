// Package core holds momo's view of a conversation: the messages channels
// deliver and the action each direction leads to.
package core

import (
	"context"
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
// Contact identifies the conversation, qualified by the channel it arrived on.
type Message struct {
	Contact string
	Content []ContentBlock
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

// Qualify returns h with every message's contact qualified by the channel it
// arrived on, so the same contact id on two channels is two conversations. A
// channel cannot misreport the qualifier because it never supplies it.
func Qualify(channel string, h Handler) Handler {
	return qualified{channel: channel, handler: h}
}

type qualified struct {
	channel string
	handler Handler
}

func (q qualified) Received(ctx context.Context, m Message, reply Reply) {
	q.handler.Received(ctx, q.qualify(m), reply)
}

func (q qualified) Sent(ctx context.Context, m Message) {
	q.handler.Sent(ctx, q.qualify(m))
}

func (q qualified) qualify(m Message) Message {
	m.Contact = q.channel + ":" + m.Contact
	return m
}
