// Package channel defines the vocabulary shared between the core pipeline
// and the channel implementations that feed it.
package channel

import (
	"fmt"
	"net/http"
)

// Message is a chat message in channel-neutral form. ContactID is opaque to
// the core; only the originating channel interprets it.
type Message struct {
	ContactID string
	Text      string
}

// Reply sends a message back over the channel it arrived on.
type Reply func(Message) error

// Handler is the core pipeline as a channel sees it. Outgoing carries replies
// sent by an operator or the agent itself, not just contact messages, and gets
// no Reply: answering one would feed the agent its own words.
type Handler interface {
	Incoming(Message, Reply)
	Outgoing(Message)
}

type Channel interface {
	// Start registers HTTP-push channels' routes on mux; polling channels ignore it.
	Start(h Handler, mux *http.ServeMux)
}

var factories = map[string]func(settings map[string]string) Channel{}

// Register is called from a channel package's init, so importing the package
// is what enables it; name must match its [channels.<name>] config section.
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
