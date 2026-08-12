package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/sourcegraph/jsonrpc2"

	acpv1 "github.com/8monkey-ai/momo/internal/acp"
	"github.com/8monkey-ai/momo/internal/core"
)

// sessionScoped reports whether a method acts on one session and therefore needs
// the session header.
func sessionScoped(method string) bool {
	return method == acpv1.MethodPrompt || method == acpv1.MethodCancel
}

// streamOf names the stream a method's response goes on: session/new is answered
// on the connection-scoped stream because the client has no session id yet.
func streamOf(method, sessionID string) string {
	if method == acpv1.MethodNewSession {
		return ""
	}
	return sessionID
}

// initializeResult adds the connection id the streamable HTTP transport needs to
// the shared result: the id belongs to this transport and to no other role.
type initializeResult struct {
	acpv1.InitializeResult
	ConnectionID string `json:"connectionId"`
}

func (e *endpoint) initialize(w http.ResponseWriter, req *jsonrpc2.Request) {
	connID := e.conns.newConnection()
	w.Header().Set(connectionHeader, connID)
	w.Header().Set("Content-Type", "application/json")
	resp := result(req.ID, initializeResult{
		ConnectionID: connID,
		InitializeResult: acpv1.InitializeResult{
			ProtocolVersion: acpv1.ProtocolVersion,
			AgentInfo:       &acpv1.Implementation{Name: "momo"},
			// momo advertises the prompt content it carries to the core
			// unchanged, and no session capability: it neither lists nor
			// resumes a session.
			AgentCapabilities: acpv1.AgentCapabilities{
				PromptCapabilities: acpv1.PromptCapabilities{
					Image:           true,
					Audio:           true,
					EmbeddedContext: true,
				},
			},
		},
	})
	// Nothing can be done if the client hung up mid-response.
	_ = json.NewEncoder(w).Encode(resp)
}

func (e *endpoint) answer(ctx context.Context, req *jsonrpc2.Request, connID, sessionID string) *jsonrpc2.Response {
	switch req.Method {
	case acpv1.MethodNewSession:
		return e.newSession(req, connID)
	case acpv1.MethodPrompt:
		return e.prompt(ctx, req, connID, sessionID)
	case acpv1.MethodCancel:
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
	return result(req.ID, acpv1.NewSessionResult{SessionID: sessionID})
}

func (e *endpoint) prompt(ctx context.Context, req *jsonrpc2.Request, connID, sessionID string) *jsonrpc2.Response {
	var p acpv1.PromptParams
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
	if err := e.core.Received(ctx, core.Message{Contact: sessionID, Content: p.Prompt}, e.reply(connID, sessionID)); err != nil {
		// None of v1's five stop reasons states that the turn failed, so a failed
		// turn travels as a JSON-RPC error. The code is internal error: the request
		// was well formed and momo is the side that could not answer it.
		return errorResponse(req.ID, jsonrpc2.CodeInternalError, "the turn failed: "+err.Error())
	}
	return result(req.ID, acpv1.PromptResult{StopReason: acpv1.StopReasonEndTurn})
}

// reply emits the blocks as session/update notifications on the stream of the
// session the prompt arrived on, one notification per block, in order. The
// connection manager is shared; the destination is what this closure holds.
func (e *endpoint) reply(connID, sessionID string) core.Reply {
	return func(_ context.Context, content []core.ContentBlock) error {
		for _, block := range content {
			notif := &jsonrpc2.Request{Method: acpv1.MethodUpdate, Notif: true}
			if err := notif.SetParams(acpv1.SessionNotification{
				SessionID: sessionID,
				Update:    acpv1.SessionUpdate{SessionUpdate: acpv1.AgentMessageChunk, Content: block},
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
