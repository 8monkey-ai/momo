package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/8monkey-ai/momo/internal/acp"
	"github.com/8monkey-ai/momo/internal/core"
)

// harness is momo's side of the ACP conversation with one subprocess: the calls
// momo makes, and the requests the harness makes of momo.
type harness struct {
	cmd  *exec.Cmd
	conn *jsonrpc2.Conn
	log  *slog.Logger

	mu         sync.Mutex
	collecting bool
	reply      []core.ContentBlock
}

// Handle answers the harness. momo advertises no client capability, so every
// method but the two below is one momo does not serve.
func (h *harness) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	switch req.Method {
	case acp.MethodUpdate:
		h.update(req)
	case acp.MethodRequestPermission:
		h.permit(ctx, conn, req)
	default:
		if req.Notif {
			return
		}
		_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
			Code:    jsonrpc2.CodeMethodNotFound,
			Message: req.Method + " is not implemented",
		})
	}
}

func (h *harness) update(req *jsonrpc2.Request) {
	var notification acp.SessionNotification
	if req.Params == nil {
		return
	}
	if err := json.Unmarshal(*req.Params, &notification); err != nil {
		h.log.Warn("the harness sent an unreadable session update", "error", err)
		return
	}
	if notification.Update.SessionUpdate != acp.AgentMessageChunk {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.collecting {
		return
	}
	h.reply = append(h.reply, notification.Update.Content)
}

// permit approves a permission request with the first option the harness offers
// that allows the action. The set of options and their order belong to the
// harness, so the kind is the only thing momo reads.
func (h *harness) permit(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	var params acp.RequestPermissionParams
	if req.Params != nil {
		if err := json.Unmarshal(*req.Params, &params); err != nil {
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    jsonrpc2.CodeInvalidParams,
				Message: err.Error(),
			})
			return
		}
	}
	outcome := acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}
	for _, option := range params.Options {
		if option.Kind == acp.AllowOnce || option.Kind == acp.AllowAlways {
			outcome = acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: option.OptionID}
			break
		}
	}
	h.log.Info("permission request answered", "outcome", outcome.Outcome, "option", outcome.OptionID)
	_ = conn.Reply(ctx, req.ID, acp.RequestPermissionResult{Outcome: outcome})
}

func (h *harness) startCollecting() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.collecting = true
}

func (h *harness) stopCollecting() []core.ContentBlock {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.collecting = false
	return h.reply
}

func (h *harness) initialize(ctx context.Context) (acp.SessionCapabilities, error) {
	var result acp.InitializeResult
	err := h.conn.Call(ctx, acp.MethodInitialize, acp.InitializeParams{
		ProtocolVersion: acp.ProtocolVersion,
		ClientInfo:      &acp.Implementation{Name: "momo"},
	}, &result)
	if err != nil {
		return acp.SessionCapabilities{}, fmt.Errorf("%s: %w", acp.MethodInitialize, err)
	}
	if result.ProtocolVersion != acp.ProtocolVersion {
		return acp.SessionCapabilities{}, fmt.Errorf("the harness speaks protocol version %d, momo speaks %d",
			result.ProtocolVersion, acp.ProtocolVersion)
	}
	return result.AgentCapabilities.SessionCapabilities, nil
}

// session resumes the session of the conversation directory, or creates one.
// momo keeps no session ids, so listing is what makes a session findable again:
// without both capabilities every turn starts a new session, and that is not an
// error.
func (h *harness) session(ctx context.Context, capabilities acp.SessionCapabilities, dir string) (string, error) {
	if capabilities.List != nil && capabilities.Resume != nil {
		if session, resumed := h.resume(ctx, dir); resumed {
			return session, nil
		}
	}
	var result acp.NewSessionResult
	err := h.conn.Call(ctx, acp.MethodNewSession, acp.NewSessionParams{
		Cwd:        dir,
		McpServers: []acp.McpServer{},
	}, &result)
	if err != nil {
		return "", fmt.Errorf("%s: %w", acp.MethodNewSession, err)
	}
	return result.SessionID, nil
}

// resume takes the first session of the first page. momo holds one session per
// conversation and the list is already limited to that conversation's directory,
// so a second page reports a condition a cursor cannot correct.
func (h *harness) resume(ctx context.Context, dir string) (string, bool) {
	var list acp.ListSessionsResult
	if err := h.conn.Call(ctx, acp.MethodListSessions, acp.ListSessionsParams{Cwd: dir}, &list); err != nil {
		h.log.Warn("listing the harness's sessions failed, starting a new session", "error", err)
		return "", false
	}
	if len(list.Sessions) == 0 {
		return "", false
	}
	session := list.Sessions[0].SessionID
	err := h.conn.Call(ctx, acp.MethodResumeSession, acp.ResumeSessionParams{SessionID: session, Cwd: dir}, nil)
	if err != nil {
		h.log.Warn("resuming the harness's session failed, starting a new session", "error", err)
		return "", false
	}
	return session, true
}

// prompt collects the streamed reply from the prompt onwards: content the
// harness streamed while momo prepared the session belongs to an earlier turn.
func (h *harness) prompt(ctx context.Context, session string, prompt []core.ContentBlock) ([]core.ContentBlock, error) {
	h.startCollecting()
	var result acp.PromptResult
	err := h.conn.Call(ctx, acp.MethodPrompt, acp.PromptParams{SessionID: session, Prompt: prompt}, &result)
	reply := h.stopCollecting()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", acp.MethodPrompt, err)
	}
	return reply, nil
}
