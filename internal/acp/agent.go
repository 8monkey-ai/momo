package acp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/8monkey-ai/momo/internal/core"
)

// protocolVersion is the ACP version momo speaks. It is the only version momo
// negotiates, so it is also the answer to every initialize: v1 requires the
// requested version when the agent supports it and the agent's latest
// otherwise, and here those are the same integer. Answering a second version
// later is a change to this one place.
const protocolVersion = 1

const (
	methodInitialize = "initialize"
	methodNewSession = "session/new"
	methodPrompt     = "session/prompt"
	methodCancel     = "session/cancel"
)

const textBlock = "text"

// initializeResult tells the client which version momo settled on, the
// connection id every later request carries, and what momo supports. Empty
// capabilities are accurate: v1 reads an omitted capability as unsupported.
// authMethods is empty because momo has no login surface of its own, which is
// v1's way of telling a client not to call authenticate or logout.
type initializeResult struct {
	ProtocolVersion   int        `json:"protocolVersion"`
	ConnectionID      string     `json:"connectionId"`
	AgentCapabilities struct{}   `json:"agentCapabilities"`
	AuthMethods       []struct{} `json:"authMethods"`
}

type newSessionResult struct {
	SessionID string `json:"sessionId"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

type promptParams struct {
	SessionID string `json:"sessionId"`
	Prompt    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"prompt"`
}

// dispatch runs one client-to-server method. sessionID is the scope the caller's
// headers resolved to, and is empty for methods that are not session-scoped.
func (e *endpoint) dispatch(ctx context.Context, c *connection, sessionID string, m message) (any, *rpcError) {
	switch m.Method {
	case methodNewSession:
		// cwd and mcpServers are accepted and ignored: momo has no filesystem to
		// work in and no MCP client, so neither can change what it does, and
		// refusing a field v1 obliges the client to send would only turn away
		// conformant clients.
		//
		// ponytail: the id momo issues here is the contact identity the core
		// sees, and today it is the only session id in play. Once the agent
		// harness lands, the upstream agent issues one of its own and the two
		// have to be mapped, with the id momo handed the client staying stable
		// across whatever the upstream does.
		id, room := c.newSession()
		if !room {
			return nil, &rpcError{Code: codeInternalError, Message: "too many sessions on this connection"}
		}
		return newSessionResult{SessionID: id}, nil
	case methodPrompt:
		return e.prompt(ctx, sessionID, m.Params)
	case methodCancel:
		// A notification with nothing to cancel: momo's turn ends inside the
		// prompt that started it, so no work outlives it.
		return nil, nil
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + m.Method}
	}
}

// prompt delivers the client's prompt to the core as a message from the contact
// the session is, and ends the turn. Nothing here reaches core.Sent: that
// direction needs an agent to produce a reply, and there is none yet.
func (e *endpoint) prompt(ctx context.Context, sessionID string, params json.RawMessage) (any, *rpcError) {
	var p promptParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "prompt parameters are malformed"}
	}
	if p.SessionID != "" && p.SessionID != sessionID {
		return nil, &rpcError{Code: codeInvalidParams, Message: "sessionId does not match " + sessionHeader}
	}
	// Blocks momo cannot read as text contribute nothing, so a prompt made only
	// of them leaves the core nothing to act on.
	if text := promptText(p); text != "" {
		e.core.Received(ctx, core.Message{Contact: sessionID, Text: text})
	}
	// stopReason is always end_turn: with no agent, the turn is over as soon as
	// the prompt has been delivered.
	return promptResult{StopReason: "end_turn"}, nil
}

func promptText(p promptParams) string {
	var texts []string
	for _, block := range p.Prompt {
		if block.Type == textBlock {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// sessionScoped reports whether a method is addressed to one session, which
// decides both the headers it must carry and the stream its response lands on.
func sessionScoped(method string) bool {
	return method == methodPrompt || method == methodCancel
}
