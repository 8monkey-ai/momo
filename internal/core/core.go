// Package core holds momo's view of a conversation: the messages channels
// deliver and the action each direction leads to.
package core

import (
	"context"
	"log/slog"
)

// Message is a message exchanged with a contact, in the shape the core acts on.
type Message struct {
	Contact string
	Content []ContentBlock
}

// ContentBlock is one ACP v1 content block. Content crosses into the core in
// ACP's own shape so a channel that speaks ACP needs no transformation.
type ContentBlock struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	URI      string            `json:"uri,omitempty"`
	Name     string            `json:"name,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

type EmbeddedResource struct {
	URI  string `json:"uri,omitempty"`
	Text string `json:"text,omitempty"`
	Blob string `json:"blob,omitempty"`
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
