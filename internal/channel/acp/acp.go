// Package acp serves the Agent Client Protocol over streamable HTTP, with momo
// as the agent: the peer that connects is the client, and a prompt it sends is
// a message from a contact.
//
// Built against the transport RFD revision of 2026-07-02 ("Streamable HTTP and
// WebSocket transport"), protocol version 1.
package acp

import (
	"context"
	"errors"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/core"
)

func init() {
	channel.Register("acp", New)
}

type settings struct {
	Token           string        `yaml:"token"`
	Path            string        `yaml:"path"`
	ConnectionGrace time.Duration `yaml:"connection_grace"`
}

type acp struct {
	routes []channel.Route
}

func (a acp) Routes() []channel.Route { return a.routes }

// New configures the ACP channel: one endpoint serving POST, GET and DELETE,
// with every request authenticated by a bearer token.
func New(lifetime context.Context, decode channel.Decoder, h core.Handler) (channel.Channel, error) {
	s := settings{Path: "/v1/acp", ConnectionGrace: 5 * time.Minute}
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.Token == "" {
		return nil, errors.New("token is required")
	}
	// A non-positive grace would panic the sweep's ticker.
	if s.ConnectionGrace <= 0 {
		return nil, errors.New("connection_grace must be positive")
	}
	conns := newConnectionManager(s.ConnectionGrace, time.Now)
	go conns.run(lifetime)
	return acp{routes: []channel.Route{
		{Path: s.Path, Handler: &endpoint{token: s.Token, conns: conns, core: h}},
	}}, nil
}
