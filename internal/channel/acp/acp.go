// Package acp serves the agent side of ACP over the streamable HTTP transport:
// the peer that connects is the client, and a prompt it sends is a message from
// a contact.
package acp

import (
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
	reg   *registry
}

func (a acp) Routes() []channel.Route { return []channel.Route{a.route} }

// Stop closes the open streams. An SSE response never ends on its own, so
// without this momo's shutdown would wait for every connected client.
func (a acp) Stop() { a.reg.stop() }

// New configures the ACP channel: one endpoint serving POST, GET and DELETE,
// every request authenticated with the operator's bearer token.
func New(decode channel.Decoder, h core.Handler) (channel.Channel, error) {
	s := settings{Path: "/acp"}
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.Token == "" {
		return nil, errors.New("token is required")
	}
	reg := newRegistry()
	return acp{
		route: channel.Route{Path: s.Path, Handler: &handler{token: s.Token, core: h, reg: reg}},
		reg:   reg,
	}, nil
}
