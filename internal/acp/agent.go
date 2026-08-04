package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/8monkey-ai/momo/internal/core"
)

// momo speaks one protocol version, so negotiation is: answer it. v1 has the
// agent answer the client's version when it supports that one and the latest it
// supports otherwise, which is this integer either way until momo learns 2.
const protocolVersion = 1

const (
	methodInitialize = "initialize"
	methodNewSession = "session/new"
	methodPrompt     = "session/prompt"
	methodCancel     = "session/cancel"
)

const (
	stopReasonEndTurn = "end_turn"
	textBlock         = "text"
)

type initializeResult struct {
	ProtocolVersion int `json:"protocolVersion"`
	// momo advertises nothing beyond the v1 baseline, and an omitted capability
	// means unsupported.
	AgentCapabilities struct{} `json:"agentCapabilities"`
	AgentInfo         struct {
		Name string `json:"name"`
	} `json:"agentInfo"`
	// v1 reads an empty list as "this agent has no login surface", which is
	// accurate: momo holds no provider credentials. Authenticating the caller is
	// the transport's job, and the bearer token does it.
	AuthMethods []any `json:"authMethods"`
	// The transport returns the connection id in the body as well as the header.
	ConnectionID string `json:"connectionId"`
}

// initialize is the one request answered in the POST response rather than on a
// stream: the client has nowhere to listen yet.
func (h *handler) initialize(w http.ResponseWriter, req request) {
	id, err := h.reg.create()
	if err != nil {
		reject{http.StatusServiceUnavailable, err.Error()}.write(w)
		return
	}
	result := initializeResult{ProtocolVersion: protocolVersion, AuthMethods: []any{}, ConnectionID: id}
	result.AgentInfo.Name = "momo"
	w.Header().Set(connectionHeader, id)
	w.Header().Set("Content-Type", jsonMediaType)
	// Nothing can be done if the client hung up mid-response.
	_, _ = w.Write(succeeded(req.ID, result))
}

func (h *handler) newSession(c *conn, req request) []byte {
	var p struct {
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return failed(req.ID, codeInvalidParams, "unreadable params")
	}
	// cwd is accepted and ignored: momo has no filesystem to work in, so holding
	// the client to an absolute path would protect nothing. mcpServers is refused
	// because momo has no MCP client, and a client that asked for those tools
	// would otherwise be told the session is ready and never learn they are absent.
	if len(p.MCPServers) > 0 {
		return failed(req.ID, codeInvalidParams, "momo connects to no MCP servers: omit mcpServers")
	}
	id, err := c.newSession()
	if err != nil {
		return failed(req.ID, codeInternalError, err.Error())
	}
	return succeeded(req.ID, map[string]string{"sessionId": id})
}

// prompt completes the turn immediately: there is no agent yet to produce
// session/update notifications or any other stop reason.
func (h *handler) prompt(ctx context.Context, sessionID string, req request) []byte {
	var p struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return failed(req.ID, codeInvalidParams, "unreadable params")
	}
	texts := make([]string, 0, len(p.Prompt))
	for _, block := range p.Prompt {
		if block.Type == textBlock {
			texts = append(texts, block.Text)
		}
	}
	// The prompt is the contact speaking, and the session is the contact. Nothing
	// here is Sent: that direction needs an agent to produce it, and there is none
	// yet. A prompt of nothing but blocks momo does not read is not a message,
	// so the core hears nothing.
	if text := strings.Join(texts, "\n"); text != "" {
		h.core.Received(ctx, core.Message{Contact: sessionID, Text: text})
	}
	return succeeded(req.ID, map[string]string{"stopReason": stopReasonEndTurn})
}
