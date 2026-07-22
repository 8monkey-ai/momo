// Package channel defines the vocabulary shared between the core pipeline
// and the messaging-channel implementations that feed it.
package channel

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
