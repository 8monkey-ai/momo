// Package channel defines the vocabulary shared between the core pipeline
// and the channel implementations that feed it.
package channel

import (
	"fmt"
	"net/http"
)

// Message is a chat message translated to channel-neutral form. ContactID is
// opaque to the core; only the originating channel interprets it.
type Message struct {
	ContactID string
	Text      string
}

// Handler is the core pipeline as a channel sees it: implementations
// translate their transport's events into Messages and deliver them here.
type Handler interface {
	// Incoming receives a message a contact sent to the workspace.
	Incoming(Message)
	// Outgoing receives a reply an operator (or the agent itself) sent.
	Outgoing(Message)
}

// Channel connects contacts on an external platform to the core pipeline.
type Channel interface {
	SendText(contactID, text string) error
}

// WebhookHandler is the capability of channels whose transport pushes events
// via HTTP callbacks; the returned handler is mounted at POST /webhook/<name>.
type WebhookHandler interface {
	Webhook(Handler) http.Handler
}

var factories = map[string]func(settings map[string]string) Channel{}

// Register makes a channel available under the given config-section name.
// Implementations call it from init, so importing a channel package is what
// makes it available.
func Register(name string, factory func(settings map[string]string) Channel) {
	factories[name] = factory
}

func New(name string, settings map[string]string) (Channel, error) {
	f, ok := factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown channel %q", name)
	}
	return f(settings), nil
}
