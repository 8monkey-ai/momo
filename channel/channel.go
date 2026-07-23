// Package channel defines the vocabulary shared between the core pipeline
// and the messaging-channel implementations that feed it.
package channel

import "net/http"

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

// WebhookReceiver is a messaging channel whose transport pushes events via
// HTTP callbacks. The returned handler is mounted at POST /webhook/<name>.
// Channels with other transports (e.g. long polling, like a direct Telegram
// bot) get their own receiver interface alongside this one.
type WebhookReceiver interface {
	// Name identifies the channel in URLs and logs, e.g. "respondio".
	Name() string
	Webhook(Handler) http.Handler
}
