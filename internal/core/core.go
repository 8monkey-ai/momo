// Package core holds momo's view of a conversation: the messages channels
// deliver and the action each direction leads to.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

const textType = "text"

// Block is one ACP content block. momo's internal representation of message
// content is ACP's own, so a block a channel receives travels unchanged and a
// channel that speaks ACP converts nothing.
//
// The fields are the ones ACP v1 requires across its five block types; the
// optional decoration, annotations and _meta, is dropped, as nothing forwards a
// block onward yet.
type Block struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Data     string          `json:"data,omitempty"`
	MimeType string          `json:"mimeType,omitempty"`
	URI      string          `json:"uri,omitempty"`
	Name     string          `json:"name,omitempty"`
	Resource json.RawMessage `json:"resource,omitempty"`
}

// Text is the content block for a message that is nothing but words.
func Text(text string) Block { return Block{Type: textType, Text: text} }

// String renders a block for a log line: what it is, and what it says when that
// is words.
func (b Block) String() string {
	if b.Text == "" {
		return b.Type
	}
	return fmt.Sprintf("%s %q", b.Type, b.Text)
}

// Message is a message exchanged with a contact, in the shape the core acts on.
type Message struct {
	Contact string
	Content []Block
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
	h.Log.Info("message received", "contact", m.Contact, "content", m.Content)
}

func (h LogHandler) Sent(_ context.Context, m Message) {
	h.Log.Info("message sent", "contact", m.Contact, "content", m.Content)
}
