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

// Message is a message exchanged with a contact, in the shape the core acts on.
type Message struct {
	Contact string
	Content []ContentBlock
}

// Handler is what a channel delivers messages to. The two directions are
// separate methods because each is an occasion for a different action.
type Handler interface {
	Received(ctx context.Context, m Message)
	Sent(ctx context.Context, m Message)
}

// LogHandler reports every message on the log.
type LogHandler struct {
	Log *slog.Logger
}

func (h LogHandler) Received(_ context.Context, m Message) {
	h.Log.Info("message received", attrs(m)...)
}

func (h LogHandler) Sent(_ context.Context, m Message) {
	h.Log.Info("message sent", attrs(m)...)
}

// attrs reports a message's block types and its text, and never the base64 data
// an image, audio or blob block carries: one of those would write megabytes into
// a single log record.
func attrs(m Message) []any {
	types := make([]string, 0, len(m.Content))
	var text []string
	for _, block := range m.Content {
		types = append(types, block.Type)
		if block.Text != "" {
			text = append(text, block.Text)
		}
	}
	return []any{"contact", m.Contact, "blocks", types, "text", strings.Join(text, " ")}
}
