// Package acp serves the agent side of ACP over the streamable HTTP transport:
// the peer that connects is the client, and a prompt it sends is a message from
// a contact.
package acp

import (
	"context"
	"errors"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/core"
)

func init() {
	channel.Register("acp", New)
}

type settings struct {
	Token string `yaml:"token"`
	Path  string `yaml:"path"`
}

type acp struct {
	route channel.Route
}

func (a acp) Routes() []channel.Route { return []channel.Route{a.route} }

// New configures the ACP channel: one endpoint serving POST, GET and DELETE,
// every request authenticated with the operator's bearer token. ctx is the
// channel's lifetime: an SSE response never ends on its own, so the streams are
// tied to it and end when momo starts shutting down. How many streams it may
// hold open is not its own business: the listener caps momo's accepted
// connections.
func New(ctx context.Context, decode channel.Decoder, h core.Handler) (channel.Channel, error) {
	s := settings{Path: "/acp"}
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.Token == "" {
		return nil, errors.New("token is required")
	}
	return acp{route: channel.Route{
		Path:    s.Path,
		Handler: &handler{token: s.Token, core: h, conns: newConnectionManager(), life: ctx},
	}}, nil
}
