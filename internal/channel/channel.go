// Package channel is the contract a messaging channel plugs into: it declares
// itself at startup, decodes its own settings, and contributes the HTTP routes
// it needs, if any. The channels themselves live in subpackages of this one.
package channel

import (
	"context"
	"fmt"
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

// Decoder fills v from the channel's block in the configuration file, leaving v
// untouched when the block is empty.
type Decoder func(v any) error

// Factory builds a channel from its configuration. The context is the
// channel's lifetime: momo cancels it when it begins shutting down, and a
// channel releases whatever it holds — open streams, goroutines, clients — when
// that happens. The process decides when that is; a channel never watches for
// it itself.
type Factory func(lifetime context.Context, decode Decoder, h core.Handler) (Channel, error)

var factories = map[string]Factory{}

// qualifying prefixes a message's conversation with the channel's configured
// name before the handler sees it. A channel never sees the qualified value, so
// it cannot omit the name and cannot report another channel's name.
type qualifying struct {
	name string
	h    core.Handler
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
func Build(lifetime context.Context, configs map[string]Decoder, h core.Handler) ([]Instance, error) {
	instances := make([]Instance, 0, len(configs))
	for _, name := range slices.Sorted(maps.Keys(configs)) {
		f, known := factories[name]
		if !known {
			return nil, fmt.Errorf("unknown channel %q, known channels: %v", name, slices.Sorted(maps.Keys(factories)))
		}
		c, err := f(lifetime, configs[name], qualifying{name: name, h: h})
		if err != nil {
			return nil, fmt.Errorf("channel %q: %w", name, err)
		}
		instances = append(instances, Instance{Name: name, Channel: c})
	}
	return instances, nil
}
