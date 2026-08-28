// Package channel is the contract a messaging channel plugs into: it declares
// itself at startup, decodes its own settings, and contributes the HTTP routes
// it needs, if any. The channels themselves live in subpackages of this one.
package channel

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"

	"github.com/8monkey-ai/momo/internal/core"
)

// Route is an HTTP endpoint a channel needs momo to serve. Channels that fetch
// their messages themselves contribute none.
type Route struct {
	Path    string
	Handler http.Handler
}

// Channel is a configured channel, ready to be served.
type Channel interface {
	Routes() []Route
}

// Decoder fills v from a block in the configuration file, leaving v untouched
// when the block is empty.
type Decoder func(v any) error

// Config is one channel's configuration: the settings the channel decodes
// itself, and the delivery block momo decodes for it. A channel never sees the
// delivery block, so its own strict decoding is unaffected by it.
type Config struct {
	Settings Decoder
	Delivery Decoder
}

// Factory builds a channel from its configuration. The context is the
// channel's lifetime: momo cancels it when it begins shutting down, and a
// channel releases whatever it holds — open streams, goroutines, clients — when
// that happens. The process decides when that is; a channel never watches for
// it itself. The history is nil when the operator configured no session history
// sync, so a channel whose settings need one refuses them.
type Factory func(lifetime context.Context, decode Decoder, h core.Handler, history core.History, files core.ConversationFiles) (Channel, error)

var factories = map[string]Factory{}

// qualifying prefixes a message's conversation with the channel's configured
// name before the handler and the history see it. A channel never sees the
// qualified value, so it cannot omit the name and cannot report another
// channel's name.
type qualifying struct {
	name    string
	h       core.Handler
	history core.History
	files   core.ConversationFiles
}

func (q qualifying) qualify(m core.Message) core.Message {
	m.Conversation = q.name + ":" + m.Conversation
	return m
}

func (q qualifying) Received(ctx context.Context, m core.Message, reply core.Reply) error {
	return q.h.Received(ctx, q.qualify(m), reply)
}

func (q qualifying) Sent(ctx context.Context, m core.Message) {
	q.h.Sent(ctx, q.qualify(m))
}

func (q qualifying) RecordUser(ctx context.Context, m core.Message) {
	q.history.RecordUser(ctx, q.qualify(m))
}

func (q qualifying) RecordAssistant(ctx context.Context, m core.Message) {
	q.history.RecordAssistant(ctx, q.qualify(m))
}

func (q qualifying) Save(ctx context.Context, conversation, name string, r io.Reader) (string, string, error) {
	return q.files.Save(ctx, q.name+":"+conversation, name, r)
}

// Register makes a channel available under the name operators use to configure
// it. It is meant to be called from a package's init.
func Register(name string, f Factory) {
	if _, dup := factories[name]; dup {
		panic("channel already registered: " + name)
	}
	factories[name] = f
}

// Instance is a built channel under the name it was configured with.
type Instance struct {
	Name    string
	Channel Channel
}

// Build builds every configured channel, in a stable order.
func Build(lifetime context.Context, configs map[string]Config, h core.Handler, history core.History, files core.ConversationFiles) ([]Instance, error) {
	instances := make([]Instance, 0, len(configs))
	for _, name := range slices.Sorted(maps.Keys(configs)) {
		f, known := factories[name]
		if !known {
			return nil, fmt.Errorf("unknown channel %q, known channels: %v", name, slices.Sorted(maps.Keys(factories)))
		}
		delivery, err := core.NewDelivery(configs[name].Delivery)
		if err != nil {
			return nil, fmt.Errorf("channel %q: %w", name, err)
		}
		q := qualifying{name: name, h: h, history: history, files: files}
		// A channel reads an absent history to refuse a setting that needs one, so a
		// wrapper around nothing must not look like one.
		var recording core.History
		if history != nil {
			recording = q
		}
		var storing core.ConversationFiles
		if files != nil {
			storing = q
		}
		c, err := f(lifetime, configs[name].Settings, delivery.Handler(q), recording, storing)
		if err != nil {
			return nil, fmt.Errorf("channel %q: %w", name, err)
		}
		instances = append(instances, Instance{Name: name, Channel: c})
	}
	return instances, nil
}
