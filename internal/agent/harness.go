package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/8monkey-ai/momo/internal/acp"
	"github.com/8monkey-ai/momo/internal/core"
)

// harness is one running agent subprocess and the ACP connection to it.
type harness struct {
	cmd     *exec.Cmd
	conn    *jsonrpc2.Conn
	stopped context.CancelFunc
	log     *slog.Logger

	mu      sync.Mutex
	content []core.ContentBlock
}

// start runs the harness in dir and connects to its stdin and stdout.
func start(parent context.Context, command []string, dir string, log *slog.Logger) (*harness, error) {
	ctx, cancel := context.WithCancel(parent)
	// The operator chooses this command in the configuration file; running it is
	// the point.
	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec // operator-supplied command
	cmd.Dir = dir
	// The stderr of the harness is the only diagnostic output a failing harness
	// has.
	cmd.Stderr = stderrLog{log: log}
	// A harness stores its session while it shuts down, so it is signalled and
	// then waited for. Wait kills it only after WaitDelay.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	h := &harness{cmd: cmd, stopped: cancel, log: log}
	// The connection lives as long as the pipes do, and stop closes it: a context
	// that ends the connection before the subprocess is waited for would lose the
	// last message the harness sent.
	h.conn = jsonrpc2.NewConn(
		context.Background(),
		jsonrpc2.NewPlainObjectStream(pipes{Reader: stdout, WriteCloser: stdin}),
		jsonrpc2.HandlerWithError(h.handle),
	)
	return h, nil
}

// stop ends the subprocess and waits for it, so the harness stores its session
// before momo lets go of the turn.
func (h *harness) stop() {
	h.stopped()
	// The harness exits on the signal, so Wait reports the signal it exited on.
	_ = h.cmd.Wait()
	_ = h.conn.Close()
}

func (h *harness) initialize(ctx context.Context) (acp.SessionCapabilities, error) {
	var res acp.InitializeResult
	params := acp.InitializeParams{ProtocolVersion: acp.Version}
	if err := h.conn.Call(ctx, acp.MethodInitialize, params, &res); err != nil {
		return acp.SessionCapabilities{}, err
	}
	if res.ProtocolVersion != acp.Version {
		return acp.SessionCapabilities{}, fmt.Errorf("the harness answered protocol version %d, momo speaks %d", res.ProtocolVersion, acp.Version)
	}
	return res.AgentCapabilities.SessionCapabilities, nil
}

// session resumes the session of the conversation, or creates one. Listing is
// what makes a session findable, and momo keeps no session id of its own, so a
// harness that advertises one of the two capabilities without the other gets a
// new session every turn.
func (h *harness) session(ctx context.Context, sessions acp.SessionCapabilities, dir string) (string, error) {
	if sessions.List != nil && sessions.Resume != nil {
		if id, resumed := h.resume(ctx, dir); resumed {
			return id, nil
		}
	}
	var res acp.NewSessionResult
	params := acp.NewSessionParams{Cwd: dir, MCPServers: []acp.MCPServer{}}
	if err := h.conn.Call(ctx, acp.MethodNewSession, params, &res); err != nil {
		return "", err
	}
	if res.SessionID == "" {
		return "", errors.New("the harness created a session with no id")
	}
	return res.SessionID, nil
}

// resume takes the first session the harness lists for the directory: one
// directory holds one conversation, so the first page is the whole answer. A
// session listed under another directory belongs to another conversation, so the
// listed cwd decides and not the order of the list. A harness that cannot list or
// cannot resume is not an error, because a new session answers the prompt.
func (h *harness) resume(ctx context.Context, dir string) (string, bool) {
	var listed acp.ListSessionsResult
	if err := h.conn.Call(ctx, acp.MethodListSessions, acp.ListSessionsParams{Cwd: dir}, &listed); err != nil {
		h.log.Warn("listing the harness sessions failed, the turn gets a new session", "error", err)
		return "", false
	}
	i := slices.IndexFunc(listed.Sessions, func(s acp.SessionInfo) bool { return s.Cwd == dir })
	if i < 0 {
		return "", false
	}
	id := listed.Sessions[i].SessionID
	params := acp.ResumeSessionParams{SessionID: id, Cwd: dir, MCPServers: []acp.MCPServer{}}
	if err := h.conn.Call(ctx, acp.MethodResumeSession, params, nil); err != nil {
		h.log.Warn("resuming the harness session failed, the turn gets a new session", "session", id, "error", err)
		return "", false
	}
	return id, true
}

// prompt sends the message and returns when the turn is over. Every stop reason
// v1 has ends a turn, so the reason is the decision of the harness and not
// momo's.
func (h *harness) prompt(ctx context.Context, session string, prompt []core.ContentBlock) error {
	params := acp.PromptParams{SessionID: session, Prompt: prompt}
	return h.conn.Call(ctx, acp.MethodPrompt, params, nil)
}

func (h *harness) handle(_ context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (any, error) {
	switch req.Method {
	case acp.MethodUpdate:
		var p acp.UpdateParams
		if err := params(req, &p); err != nil {
			return nil, err
		}
		if p.Update.SessionUpdate == acp.AgentMessageChunk {
			h.append(p.Update.Content)
		}
		return nil, nil
	case acp.MethodRequestPermission:
		var p acp.RequestPermissionParams
		if err := params(req, &p); err != nil {
			return nil, err
		}
		return acp.RequestPermissionResult{Outcome: approve(p.Options)}, nil
	default:
		// momo holds no editor state and no terminal, and advertises neither, so a
		// request for one is a method momo does not have.
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: req.Method + " is not implemented"}
	}
}

// approve selects an option that allows the operation. The harness supplies the
// list, so the kind of an option decides and never a fixed set of names. A list
// with nothing to allow is cancelled.
func approve(options []acp.PermissionOption) acp.PermissionOutcome {
	for _, kind := range []string{acp.KindAllowOnce, acp.KindAllowAlways} {
		for _, option := range options {
			if option.Kind == kind {
				return acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: option.OptionID}
			}
		}
	}
	return acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}
}

func (h *harness) append(block core.ContentBlock) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.content = append(h.content, block)
}

func (h *harness) discard() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.content = nil
}

func (h *harness) collected() []core.ContentBlock {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.content)
}

func params(req *jsonrpc2.Request, v any) error {
	if req.Params == nil {
		return errors.New("params are required")
	}
	return json.Unmarshal(*req.Params, v)
}

// pipes joins the two halves of a subprocess into the one read-write-closer the
// JSON-RPC stream needs: momo reads what the harness writes, and writes what the
// harness reads.
type pipes struct {
	io.Reader
	io.WriteCloser
}

// stderrLog sends what the harness writes to its stderr to the momo log.
type stderrLog struct {
	log *slog.Logger
}

func (s stderrLog) Write(p []byte) (int, error) {
	s.log.Info("harness stderr", "output", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
