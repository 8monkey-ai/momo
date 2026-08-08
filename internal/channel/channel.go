// Package channel is the contract a messaging channel plugs into: it declares
// itself at startup, decodes its own settings, and contributes the HTTP routes
// it needs, if any.
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

// Factory builds a channel from its configuration. ctx is the channel's
// lifetime: it is cancelled when momo begins shutting down, before the server
// drains its in-flight requests, so a channel holding something that does not
// end on its own — a response, a goroutine, an open socket — lets go of it
// there. A channel that holds nothing long-lived ignores it.
type Factory func(ctx context.Context, decode Decoder, h core.Handler) (Channel, error)

var factories = map[string]Factory{}

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
func Build(ctx context.Context, configs map[string]Decoder, h core.Handler) ([]Instance, error) {
	instances := make([]Instance, 0, len(configs))
	for _, name := range slices.Sorted(maps.Keys(configs)) {
		f, known := factories[name]
		if !known {
			return nil, fmt.Errorf("unknown channel %q, known channels: %v", name, slices.Sorted(maps.Keys(factories)))
		}
		c, err := f(ctx, configs[name], h)
		if err != nil {
			return nil, fmt.Errorf("channel %q: %w", name, err)
		}
		instances = append(instances, Instance{Name: name, Channel: c})
	}
	return instances, nil
}
