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
	"time"

	"github.com/sourcegraph/jsonrpc2"

	wire "github.com/8monkey-ai/momo/internal/acp"
	"github.com/8monkey-ai/momo/internal/core"
)

// run performs the turn on a subprocess of its own: initialize, a new session in
// the conversation's directory, and the prompt. Nothing is tried a second time.
func (h *Harness) run(ctx context.Context, dir string, prompt []core.ContentBlock) ([]core.ContentBlock, error) {
	p, err := h.start(ctx, dir)
	if err != nil {
		return nil, err
	}
	// Every exit of the turn, an error included, stops the subprocess.
	defer p.stop()
	if err := p.initialize(ctx); err != nil {
		return nil, err
	}
	sessionID, err := p.newSession(ctx, dir)
	if err != nil {
		return nil, err
	}
	return p.prompt(ctx, sessionID, prompt)
}

// process is one agent subprocess and the ACP connection to it.
type process struct {
	log  *slog.Logger
	cmd  *exec.Cmd
	conn *jsonrpc2.Conn
	// end stops the process. Its context is separate from the turn's, so the
	// turn's deadline cannot kill the agent before momo has stopped it.
	end context.CancelFunc

	mu         sync.Mutex
	collecting bool
	collected  []core.ContentBlock
}

func (h *Harness) start(ctx context.Context, dir string) (*process, error) {
	processCtx, end := context.WithCancel(context.WithoutCancel(ctx))
	// The operator chooses the command; running it is the point.
	cmd := exec.CommandContext(processCtx, h.command[0], h.command[1:]...) //nolint:gosec // operator-supplied command
	cmd.Dir = dir
	// An interrupt lets the agent store its session; the delay is the wait before
	// the kill.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	diag := diagnostics{log: h.log}
	cmd.Stderr = diag
	stdin, err := cmd.StdinPipe()
	if err != nil {
		end()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		end()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		end()
		return nil, fmt.Errorf("starting the agent: %w", err)
	}
	p := &process{log: h.log, cmd: cmd, end: end}
	p.conn = jsonrpc2.NewConn(processCtx, jsonrpc2.NewPlainObjectStream(pipes{stdout, stdin}), p.handler(), jsonrpc2.SetLogger(diag))
	return p, nil
}

func (p *process) stop() {
	if err := p.conn.Close(); err != nil && !errors.Is(err, jsonrpc2.ErrClosed) {
		p.log.Warn("closing the agent connection", "error", err)
	}
	p.end()
	if err := p.cmd.Wait(); err != nil {
		p.log.Info("the agent exited", "error", err)
	}
}

func (p *process) initialize(ctx context.Context) error {
	var res wire.InitializeResult
	params := wire.InitializeParams{ProtocolVersion: wire.ProtocolVersion}
	if err := p.conn.Call(ctx, wire.MethodInitialize, params, &res); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if res.ProtocolVersion != wire.ProtocolVersion {
		return fmt.Errorf("the agent speaks protocol version %d, momo speaks %d", res.ProtocolVersion, wire.ProtocolVersion)
	}
	return nil
}

func (p *process) newSession(ctx context.Context, dir string) (string, error) {
	var res wire.NewSessionResult
	params := wire.NewSessionParams{Cwd: dir, McpServers: []any{}}
	if err := p.conn.Call(ctx, wire.MethodNewSession, params, &res); err != nil {
		return "", fmt.Errorf("session/new: %w", err)
	}
	if res.SessionID == "" {
		return "", errors.New("session/new returned no session id")
	}
	return res.SessionID, nil
}

// prompt collects the streamed content from the prompt onwards, so content the
// agent streamed while momo prepared the session is not in this turn's reply.
func (p *process) prompt(ctx context.Context, sessionID string, content []core.ContentBlock) ([]core.ContentBlock, error) {
	p.mu.Lock()
	p.collecting = true
	p.mu.Unlock()
	var res wire.PromptResult
	params := wire.PromptParams{SessionID: sessionID, Prompt: content}
	if err := p.conn.Call(ctx, wire.MethodPrompt, params, &res); err != nil {
		return nil, fmt.Errorf("session/prompt: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.collected, nil
}

// handler answers what the agent sends momo: the chunks of its message, and
// method-not-found for everything momo does not serve.
func (p *process) handler() jsonrpc2.Handler {
	return jsonrpc2.HandlerWithError(func(_ context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (any, error) {
		if req.Method == wire.MethodUpdate {
			return nil, p.update(req)
		}
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: req.Method + " is not supported"}
	})
}

func (p *process) update(req *jsonrpc2.Request) error {
	if req.Params == nil {
		return errors.New("session/update requires params")
	}
	var params wire.UpdateParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return err
	}
	if params.Update.SessionUpdate != wire.SessionUpdateAgentMessageChunk {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.collecting {
		return nil
	}
	p.collected = append(p.collected, params.Update.Content)
	return nil
}

// pipes joins the stdout and the stdin of a subprocess into the one stream the
// JSON-RPC framing reads and writes.
type pipes struct {
	io.ReadCloser
	io.WriteCloser
}

func (p pipes) Close() error {
	err := p.WriteCloser.Close()
	if other := p.ReadCloser.Close(); err == nil {
		err = other
	}
	return err
}

// diagnostics puts the stderr of the agent and the complaints of the JSON-RPC
// connection in momo's log. It is the only diagnostic output a failing harness
// has. momo writes no message content of its own here; what the agent writes to
// its stderr is the agent's decision.
type diagnostics struct{ log *slog.Logger }

func (d diagnostics) Write(b []byte) (int, error) {
	d.log.Info("agent output", "output", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

func (d diagnostics) Printf(format string, v ...any) {
	d.log.Info("agent connection", "message", strings.TrimRight(fmt.Sprintf(format, v...), "\n"))
}
