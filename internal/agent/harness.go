package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/8monkey-ai/momo/internal/acp"
	"github.com/8monkey-ai/momo/internal/core"
)

// harness is one spawned subprocess and the ACP connection to it. It serves the
// turn it was spawned for and nothing after it.
type harness struct {
	cmd  *exec.Cmd
	conn *jsonrpc2.Conn
	log  *slog.Logger

	mu sync.Mutex
	// collecting is true from the prompt onward: what the agent streams while the
	// session is being set up belongs to an earlier turn.
	collecting bool
	content    []core.ContentBlock
}

func (a *Agent) spawn(ctx context.Context, dir string) (*harness, error) {
	// The command comes from the configuration file, which is the operator naming
	// the harness to run.
	cmd := exec.CommandContext(ctx, a.command[0], a.command[1:]...) //nolint:gosec // operator-supplied command
	cmd.Dir = dir
	cmd.Stderr = diagnostics{log: a.log}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawning %s: %w", a.command[0], err)
	}
	h := &harness{cmd: cmd, log: a.log}
	h.conn = jsonrpc2.NewConn(ctx, jsonrpc2.NewPlainObjectStream(stdio{in: stdin, out: stdout}), h)
	return h, nil
}

// turn negotiates the protocol, continues or starts this directory's session,
// prompts once, and reports what the agent streamed in answer.
func (h *harness) turn(ctx context.Context, dir string, prompt []core.ContentBlock) ([]core.ContentBlock, error) {
	var negotiated acp.InitializeResult
	params := acp.InitializeParams{ProtocolVersion: acp.Version, ClientInfo: acp.Info{Name: "momo"}}
	if err := h.conn.Call(ctx, acp.MethodInitialize, params, &negotiated); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if negotiated.ProtocolVersion != acp.Version {
		return nil, fmt.Errorf("the harness speaks protocol version %d, momo speaks %d",
			negotiated.ProtocolVersion, acp.Version)
	}
	sessionID, err := h.session(ctx, negotiated.AgentCapabilities.SessionCapabilities, dir)
	if err != nil {
		return nil, err
	}
	return h.prompt(ctx, sessionID, prompt)
}

// session continues the one session this directory holds, or starts one. momo
// keeps no session ids, so the agent's own listing is what makes a session
// findable, and an agent that cannot both list and resume gets a new session
// every turn.
func (h *harness) session(ctx context.Context, capabilities acp.SessionCapabilities, dir string) (string, error) {
	if capabilities.List != nil && capabilities.Resume != nil {
		if resumed := h.resume(ctx, dir); resumed != "" {
			return resumed, nil
		}
	}
	var created acp.NewSessionResult
	if err := h.conn.Call(ctx, acp.MethodNewSession, acp.Session("", dir), &created); err != nil {
		return "", fmt.Errorf("%s: %w", acp.MethodNewSession, err)
	}
	if created.SessionID == "" {
		return "", fmt.Errorf("%s: the harness returned no session id", acp.MethodNewSession)
	}
	return created.SessionID, nil
}

// resume reports the session it reconnected to, or the empty string when there is
// none: a listing that names nothing for this directory, and a resumption the
// agent refuses, both lead to a new session rather than a failed turn.
func (h *harness) resume(ctx context.Context, dir string) string {
	// The listing is filtered by this conversation's own directory and one session
	// per conversation is the supported behaviour, so the first page is the whole
	// answer: a second page would mean that assumption is already broken.
	var listed acp.ListSessionsResult
	if err := h.conn.Call(ctx, acp.MethodListSessions, acp.ListSessionsParams{Cwd: dir}, &listed); err != nil {
		h.log.Warn("listing the harness sessions failed, starting a new one", "error", err)
		return ""
	}
	if len(listed.Sessions) == 0 {
		return ""
	}
	sessionID := listed.Sessions[0].SessionID
	// session/resume reconnects without replay, which is why momo never calls
	// session/load: a replayed conversation would arrive as this turn's reply.
	if err := h.conn.Call(ctx, acp.MethodResumeSession, acp.Session(sessionID, dir), &struct{}{}); err != nil {
		h.log.Warn("resuming the session failed, starting a new one", "error", err)
		return ""
	}
	return sessionID
}

func (h *harness) prompt(ctx context.Context, sessionID string, prompt []core.ContentBlock) ([]core.ContentBlock, error) {
	h.mu.Lock()
	h.collecting = true
	h.mu.Unlock()

	params := acp.PromptParams{SessionID: sessionID, Prompt: prompt}
	if err := h.conn.Call(ctx, acp.MethodPrompt, params, &acp.PromptResult{}); err != nil {
		return nil, fmt.Errorf("%s: %w", acp.MethodPrompt, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.collecting = false
	return h.content, nil
}

// Handle serves the two things a harness asks of momo: the stream of the agent's
// own message, and permission to act. Everything else is method-not-found, which
// is what the client capabilities momo leaves out already say.
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
			Message: req.Method + " is not served",
		})
	}
}

func (h *harness) update(req *jsonrpc2.Request) {
	var params acp.UpdateParams
	if err := unmarshal(req, &params); err != nil {
		h.log.Warn("unreadable session/update from the harness", "error", err)
		return
	}
	if params.Update.SessionUpdate != acp.AgentMessageChunk {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.collecting {
		h.content = append(h.content, params.Update.Content)
	}
}

// permit approves what the harness asks to do. momo has nobody to ask, and the
// options are the agent's own, so one the agent itself labelled as allowing is
// taken as it stands; an agent that offers no such option gets a cancellation.
func (h *harness) permit(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	var params acp.RequestPermissionParams
	if err := unmarshal(req, &params); err != nil {
		_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
			Code:    jsonrpc2.CodeInvalidParams,
			Message: err.Error(),
		})
		return
	}
	outcome := acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}
	for _, option := range params.Options {
		if option.Kind == acp.KindAllowOnce || option.Kind == acp.KindAllowAlways {
			outcome = acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: option.OptionID}
			break
		}
	}
	if outcome.Outcome == acp.OutcomeCancelled {
		h.log.Warn("the harness offered no option that allows, cancelling")
	}
	_ = conn.Reply(ctx, req.ID, acp.RequestPermissionResult{Outcome: outcome})
}

// stop ends the harness's process. Termination is signalled rather than forced so
// the harness can persist the session momo looks for next turn, and forced only
// if it does not leave on its own.
func (h *harness) stop() {
	// Closing the connection gives the harness EOF on its stdin, which is how it
	// learns momo is done with it.
	_ = h.conn.Close()
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		h.log.Warn("signalling the harness failed", "error", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- h.cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			h.log.Debug("the harness exited", "error", err)
		}
	case <-time.After(10 * time.Second):
		h.log.Warn("the harness did not exit after being signalled, killing it", "pid", h.cmd.Process.Pid)
		_ = h.cmd.Process.Kill()
		<-exited
	}
}

func unmarshal(req *jsonrpc2.Request, v any) error {
	if req.Params == nil {
		return errors.New("params are required")
	}
	return json.Unmarshal(*req.Params, v)
}

// stdio joins a subprocess's two pipes into the single stream JSON-RPC framing
// expects. This is the only place the two shapes meet.
type stdio struct {
	in  io.WriteCloser
	out io.ReadCloser
}

func (s stdio) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s stdio) Write(p []byte) (int, error) { return s.in.Write(p) }

func (s stdio) Close() error {
	err := s.in.Close()
	if outErr := s.out.Close(); err == nil {
		err = outErr
	}
	return err
}

// diagnostics carries the harness's stderr into momo's log: it is the only
// diagnostic channel a failing harness has. Message payloads never come this
// way, so nothing here has to be held back.
type diagnostics struct {
	log *slog.Logger
}

func (d diagnostics) Write(p []byte) (int, error) {
	d.log.Warn("harness stderr", "output", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
