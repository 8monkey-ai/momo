package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/sourcegraph/jsonrpc2"

	wire "github.com/8monkey-ai/momo/internal/acp"
	"github.com/8monkey-ai/momo/internal/core"
)

// sessionScoped reports whether a method acts on one session and therefore needs
// the session header.
func sessionScoped(method string) bool {
	return method == wire.MethodPrompt || method == wire.MethodCancel
}

// streamOf names the stream a method's response goes on: session/new is answered
// on the connection-scoped stream because the client has no session id yet.
func streamOf(method, sessionID string) string {
	if method == wire.MethodNewSession {
		return ""
	}
	return sessionID
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AgentInfo         agentInfo         `json:"agentInfo"`
	ConnectionID      string            `json:"connectionId"`
}

// agentCapabilities omits authMethods and every other capability momo does not
// have: v1 reads an omitted capability as unsupported.
type agentCapabilities struct {
	PromptCapabilities promptCapabilities `json:"promptCapabilities"`
}

// promptCapabilities is accurate because momo carries every block type to the
// core unchanged rather than reading it.
type promptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type agentInfo struct {
	Name string `json:"name"`
}

func (e *endpoint) initialize(w http.ResponseWriter, req *jsonrpc2.Request) {
	connID := e.conns.newConnection()
	w.Header().Set(connectionHeader, connID)
	w.Header().Set("Content-Type", "application/json")
	resp := result(req.ID, initializeResult{
		ProtocolVersion: wire.ProtocolVersion,
		AgentInfo:       agentInfo{Name: "momo"},
		ConnectionID:    connID,
		AgentCapabilities: agentCapabilities{PromptCapabilities: promptCapabilities{
			Image:           true,
			Audio:           true,
			EmbeddedContext: true,
		}},
	})
	// Nothing can be done if the client hung up mid-response.
	_ = json.NewEncoder(w).Encode(resp)
}

func (e *endpoint) answer(ctx context.Context, req *jsonrpc2.Request, connID, sessionID string) *jsonrpc2.Response {
	switch req.Method {
	case wire.MethodNewSession:
		return e.newSession(req, connID)
	case wire.MethodPrompt:
		return e.prompt(ctx, req, connID, sessionID)
	case wire.MethodCancel:
		// Nothing is running to cancel: session/prompt completes before it is
		// answered. As a notification it is not answered at all.
		if req.Notif {
			return nil
		}
		return result(req.ID, struct{}{})
	default:
		return errorResponse(req.ID, jsonrpc2.CodeMethodNotFound, req.Method+" is not implemented")
	}
}

// newSession accepts and ignores cwd and mcpServers: momo has no filesystem to
// work in and advertises no MCP capability, so neither can be acted on, and
// refusing them would refuse a client that did nothing wrong.
func (e *endpoint) newSession(req *jsonrpc2.Request, connID string) *jsonrpc2.Response {
	sessionID, known := e.conns.newSession(connID)
	if !known {
		return errorResponse(req.ID, jsonrpc2.CodeInternalError, "the connection was released")
	}
	return result(req.ID, wire.NewSessionResult{SessionID: sessionID})
}

func (e *endpoint) prompt(ctx context.Context, req *jsonrpc2.Request, connID, sessionID string) *jsonrpc2.Response {
	var p wire.PromptParams
	if err := params(req, &p); err != nil {
		return errorResponse(req.ID, jsonrpc2.CodeInvalidParams, err.Error())
	}
	if len(p.Prompt) == 0 {
		return errorResponse(req.ID, jsonrpc2.CodeInvalidParams, "prompt must carry at least one content block")
	}
	for _, block := range p.Prompt {
		if block.Type == "" {
			return errorResponse(req.ID, jsonrpc2.CodeInvalidParams, "every content block must have a type")
		}
	}
	// The client's prompt is the contact speaking, and momo issues the session
	// id, so the session is the contact. Received returns once the reply has been
	// emitted, so the turn is answered after its content, as v1 requires.
	if err := e.core.Received(ctx, core.Message{Conversation: sessionID, Content: p.Prompt}, e.reply(connID, sessionID)); err != nil {
		return errorResponse(req.ID, jsonrpc2.CodeInternalError, err.Error())
	}
	return result(req.ID, wire.PromptResult{StopReason: wire.StopReasonEndTurn})
}

// reply emits the blocks as session/update notifications on the stream of the
// session the prompt arrived on, one notification per block, in order. Each call
// carries a message id of its own, so the client reads one paced paragraph as one
// message and the blocks of one call as that message's chunks. The connection
// manager is shared; the destination is what this closure holds.
func (e *endpoint) reply(connID, sessionID string) core.Reply {
	return func(_ context.Context, content []core.ContentBlock) error {
		messageID := newID()
		for _, block := range content {
			notif := &jsonrpc2.Request{Method: wire.MethodUpdate, Notif: true}
			if err := notif.SetParams(wire.UpdateParams{
				SessionID: sessionID,
				Update: wire.Update{
					SessionUpdate: wire.SessionUpdateAgentMessageChunk,
					MessageID:     messageID,
					Content:       block,
				},
			}); err != nil {
				return err
			}
			if !e.conns.send(connID, sessionID, frame(notif)) {
				return fmt.Errorf("session %s: nothing is listening to the session stream", sessionID)
			}
		}
		return nil
	}
}

func params(req *jsonrpc2.Request, v any) error {
	if req.Params == nil {
		return errors.New("params are required")
	}
	return json.Unmarshal(*req.Params, v)
}

func result(id jsonrpc2.ID, v any) *jsonrpc2.Response {
	resp := &jsonrpc2.Response{ID: id}
	if err := resp.SetResult(v); err != nil {
		return errorResponse(id, jsonrpc2.CodeInternalError, "the result could not be encoded")
	}
	return resp
}

func errorResponse(id jsonrpc2.ID, code int64, message string) *jsonrpc2.Response {
	return &jsonrpc2.Response{ID: id, Error: &jsonrpc2.Error{Code: code, Message: message}}
}
