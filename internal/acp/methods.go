package acp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/8monkey-ai/momo/internal/core"
)

// momo speaks protocol version 1: the only stable version, and the whole of
// what it answers. Negotiation is this constant — a client asking for a later
// version is told the latest momo supports, which is this one.
const protocolVersion = 1

const (
	methodInitialize = "initialize"
	methodNewSession = "session/new"
	methodPrompt     = "session/prompt"
	methodCancel     = "session/cancel"
)

// sessionScoped are the methods whose request must name a session.
var sessionScoped = map[string]bool{methodPrompt: true, methodCancel: true}

type initializeResult struct {
	ProtocolVersion int `json:"protocolVersion"`
	// momo supports nothing beyond v1's baseline, and has no credentials of its
	// own to log in with, so it advertises no capability and no authMethods.
	AgentCapabilities struct{}  `json:"agentCapabilities"`
	AgentInfo         agentInfo `json:"agentInfo"`
	ConnectionID      string    `json:"connectionId"`
}

type agentInfo struct {
	Name string `json:"name"`
}

type newSessionResult struct {
	SessionID string `json:"sessionId"`
}

type promptParams struct {
	Prompt []core.ContentBlock `json:"prompt"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

// initialize is the one method answered in the POST's own response, because the
// client has no stream to be answered on until it has the connection id.
func (a *acp) initialize(w http.ResponseWriter, req request) {
	id := a.conns.open()
	w.Header().Set(connectionHeader, id)
	writeJSON(w, http.StatusOK, response{JSONRPC: jsonrpcVersion, ID: req.ID, Result: initializeResult{
		ProtocolVersion: protocolVersion,
		AgentInfo:       agentInfo{Name: "momo"},
		ConnectionID:    id,
	}})
}

// call runs the method and returns what to answer with, or nothing at all when
// the message was a notification.
func (a *acp) call(ctx context.Context, req request, connID, sessionID string) (any, *rpcError) {
	switch req.Method {
	case methodNewSession:
		return a.newSession(connID)
	case methodPrompt:
		return a.prompt(ctx, sessionID, req.Params)
	case methodCancel:
		// There is no agent yet, so no turn is ever in flight to cancel, and a
		// notification is answered with nothing.
		return nil, nil
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
}

// newSession takes v1's cwd and mcpServers and ignores them: momo has no
// filesystem to work in and no MCP client.
func (a *acp) newSession(connID string) (any, *rpcError) {
	// ponytail: momo issues the session id, and today it is the only session id
	// in play. Once the agent harness lands, the upstream agent will issue one of
	// its own and the two will have to be mapped, because the id momo handed the
	// client has to stay stable whatever the upstream does.
	id, ok := a.conns.openSession(connID)
	if !ok {
		return nil, &rpcError{Code: codeInvalidRequest, Message: "unknown connection"}
	}
	return newSessionResult{SessionID: id}, nil
}

// prompt carries the client's content blocks to the core unchanged: the session
// is the contact, and the blocks are the message.
func (a *acp) prompt(ctx context.Context, sessionID string, params json.RawMessage) (any, *rpcError) {
	var p promptParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "prompt could not be read: " + err.Error()}
	}
	if len(p.Prompt) == 0 {
		return nil, &rpcError{Code: codeInvalidParams, Message: "prompt must carry at least one content block"}
	}
	for _, block := range p.Prompt {
		if block.Type == "" {
			return nil, &rpcError{Code: codeInvalidParams, Message: "every content block must carry a type"}
		}
	}
	// ponytail: nothing in this slice is Sent. The other direction needs an agent
	// to produce a reply, and there is none yet.
	a.core.Received(ctx, core.Message{Contact: sessionID, Content: p.Prompt})
	// The turn ends where it began while there is no agent to run it.
	return promptResult{StopReason: "end_turn"}, nil
}
