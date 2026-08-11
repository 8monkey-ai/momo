package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/8monkey-ai/momo/internal/core"
)

// session is one harness subprocess and the one ACP session it runs for the
// conversation. It answers the harness's calls itself, so it is the client the
// JSON-RPC connection dispatches to.
type session struct {
	cmd       *exec.Cmd
	conn      *jsonrpc2.Conn
	log       *slog.Logger
	stopGrace time.Duration
	id        string

	mu      sync.Mutex
	content []core.ContentBlock
}

// spawn starts the harness in cwd and leaves it on the conversation's session:
// the one the harness lists for that directory, or a new one when it lists
// none, cannot list, or cannot load.
func (h *Harness) spawn(ctx context.Context, conversation, cwd string) (*session, error) {
	// The operator names the harness in the configuration file; running it is the
	// point. The turn's context bounds the calls momo makes, not the process:
	// ending the process is stop's job, so a cancelled turn still gets the chance
	// to persist its session.
	cmd := exec.Command(h.command[0], h.command[1:]...) //nolint:gosec // operator-supplied command
	cmd.Dir = cwd
	cmd.Stderr = diagnostics{log: h.log, conversation: conversation}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawning the harness: %w", err)
	}

	s := &session{cmd: cmd, log: h.log, stopGrace: h.stopGrace}
	// jsonrpc2 reads and writes one stream while a subprocess offers two pipes,
	// so they are joined here, at the edge of the package.
	s.conn = jsonrpc2.NewConn(ctx, jsonrpc2.NewPlainObjectStream(stdio{out: stdout, in: stdin}), s)

	capabilities, err := s.initialize(ctx)
	if err != nil {
		s.stop()
		return nil, err
	}
	if s.id = s.resume(ctx, capabilities, cwd); s.id == "" {
		if s.id, err = s.create(ctx, cwd); err != nil {
			s.stop()
			return nil, err
		}
	}
	return s, nil
}

func (s *session) initialize(ctx context.Context) (agentCapabilities, error) {
	var res initializeResult
	params := initializeParams{ProtocolVersion: protocolVersion}
	if err := s.conn.Call(ctx, methodInitialize, params, &res); err != nil {
		return agentCapabilities{}, fmt.Errorf("%s: %w", methodInitialize, err)
	}
	if res.ProtocolVersion != protocolVersion {
		return agentCapabilities{}, fmt.Errorf("the harness speaks protocol version %d, momo speaks %d", res.ProtocolVersion, protocolVersion)
	}
	return res.AgentCapabilities, nil
}

// resume returns the session the harness loaded for cwd, or "" when the
// conversation has to start a new one. Listing is what makes a session findable:
// momo keeps no session ids of its own, so a harness that loads but does not list
// has nothing momo could ask it to load.
func (s *session) resume(ctx context.Context, capabilities agentCapabilities, cwd string) string {
	if !capabilities.LoadSession || capabilities.Session.List == nil {
		return ""
	}
	id := s.latest(ctx, cwd)
	if id == "" {
		return ""
	}
	params := loadSessionParams{SessionID: id, Cwd: cwd, McpServers: []any{}}
	if err := s.conn.Call(ctx, methodLoadSession, params, nil); err != nil {
		s.log.Warn("resuming the session failed, starting a new one", "session", id, "error", err)
		return ""
	}
	return id
}

// latest is the most recently updated session the harness lists for cwd, compared
// as the ISO 8601 strings v1 specifies. A harness that fails to list, or reports
// no timestamps, leaves the conversation to a new session.
func (s *session) latest(ctx context.Context, cwd string) string {
	var newest sessionInfo
	params := listSessionsParams{Cwd: cwd}
	for {
		var res listSessionsResult
		if err := s.conn.Call(ctx, methodListSession, params, &res); err != nil {
			s.log.Warn("listing sessions failed, starting a new one", "error", err)
			return ""
		}
		for _, info := range res.Sessions {
			if info.UpdatedAt >= newest.UpdatedAt {
				newest = info
			}
		}
		if res.NextCursor == nil {
			return newest.SessionID
		}
		params.Cursor = res.NextCursor
	}
}

func (s *session) create(ctx context.Context, cwd string) (string, error) {
	var res newSessionResult
	if err := s.conn.Call(ctx, methodNewSession, newSessionParams{Cwd: cwd, McpServers: []any{}}, &res); err != nil {
		return "", fmt.Errorf("%s: %w", methodNewSession, err)
	}
	return res.SessionID, nil
}

// prompt runs the turn and returns everything the harness streamed while it ran.
func (s *session) prompt(ctx context.Context, content []core.ContentBlock) ([]core.ContentBlock, error) {
	if err := s.conn.Call(ctx, methodPrompt, promptParams{SessionID: s.id, Prompt: content}, nil); err != nil {
		return nil, fmt.Errorf("%s: %w", methodPrompt, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.content, nil
}

// Handle answers the harness. Only the agent's message chunks and its permission
// requests are served; every other method is method-not-found, which is what an
// unadvertised capability means.
func (s *session) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	switch req.Method {
	case methodUpdate:
		s.collect(req)
	case methodPermission:
		var p permissionParams
		if err := unmarshal(req, &p); err != nil {
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: err.Error()})
			return
		}
		_ = conn.Reply(ctx, req.ID, permissionResult{Outcome: approve(p.Options)})
	default:
		if !req.Notif {
			_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
				Code:    jsonrpc2.CodeMethodNotFound,
				Message: req.Method + " is not implemented",
			})
		}
	}
}

func (s *session) collect(req *jsonrpc2.Request) {
	var p updateParams
	if err := unmarshal(req, &p); err != nil {
		return
	}
	if p.Update.SessionUpdate != updateAgentMessage {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.content = append(s.content, p.Update.Content)
}

// stop ends the subprocess without discarding the session it is holding: the
// closed stdio and the signal both ask it to shut down, and it is killed only if
// it does not take the invitation.
func (s *session) stop() {
	_ = s.conn.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
	}
	exited := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(s.stopGrace):
		s.log.Warn("the harness ignored termination, killing it", "grace", s.stopGrace)
		_ = s.cmd.Process.Kill()
		<-exited
	}
}

// stdio is the subprocess's two pipes as the single stream jsonrpc2 reads and
// writes.
type stdio struct {
	out io.ReadCloser
	in  io.WriteCloser
}

func (p stdio) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p stdio) Write(b []byte) (int, error) { return p.in.Write(b) }

func (p stdio) Close() error {
	// Closing stdin first is what tells the harness to shut down.
	err := p.in.Close()
	if closed := p.out.Close(); err == nil {
		err = closed
	}
	return err
}

// diagnostics carries the harness's stderr into momo's log: it is the only
// account a failing harness can give of itself.
type diagnostics struct {
	log          *slog.Logger
	conversation string
}

func (d diagnostics) Write(b []byte) (int, error) {
	for line := range strings.Lines(string(b)) {
		d.log.Info("harness", "conversation", d.conversation, "stderr", strings.TrimRight(line, "\n"))
	}
	return len(b), nil
}

func unmarshal(req *jsonrpc2.Request, v any) error {
	if req.Params == nil {
		return fmt.Errorf("%s: params are required", req.Method)
	}
	return json.Unmarshal(*req.Params, v)
}
