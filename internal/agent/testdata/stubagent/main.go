// Command stubagent is a minimal ACP v1 agent over stdio, for the tests of the
// agent package only. It shows initialize, session creation, session listing,
// session resumption, a prompt, a streamed reply and a permission request. Its
// reply is two agent_message_chunk blocks: the session id, then the text of the
// prompt.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/8monkey-ai/momo/internal/acp"
	"github.com/8monkey-ai/momo/internal/core"
)

type stub struct {
	noSessionCapabilities bool
	streamOnResume        bool
	askPermission         bool
	failPrompt            bool
	neverAnswerPrompt     bool
}

func main() {
	var s stub
	flag.BoolVar(&s.noSessionCapabilities, "no-session-capabilities", false, "advertise neither listing nor resumption")
	flag.BoolVar(&s.streamOnResume, "stream-on-resume", false, "stream the stored prompts of the session while resuming it")
	flag.BoolVar(&s.askPermission, "ask-permission", false, "request permission before the reply")
	flag.BoolVar(&s.failPrompt, "fail-prompt", false, "answer session/prompt with an error")
	flag.BoolVar(&s.neverAnswerPrompt, "never-answer-prompt", false, "leave session/prompt unanswered")
	flag.Parse()

	if err := writePID(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stream := jsonrpc2.NewPlainObjectStream(stdio{os.Stdin, os.Stdout})
	conn := jsonrpc2.NewConn(context.Background(), stream, jsonrpc2.AsyncHandler(&s))
	<-conn.DisconnectNotify()
}

// writePID lets a test see whether this process is still alive after the turn.
func writePID() error {
	return os.WriteFile("agent.pid", []byte(fmt.Sprintln(os.Getpid())), 0o600)
}

// stdio joins the process's separate standard input and output into the one
// read-write-closer a plain object stream reads and writes.
type stdio struct {
	io.Reader
	io.Writer
}

func (stdio) Close() error { return nil }

func (s *stub) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	result, err := s.answer(ctx, conn, req)
	switch {
	case errors.Is(err, errNoAnswer):
		return
	case err != nil:
		_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{Code: jsonrpc2.CodeInternalError, Message: err.Error()})
	default:
		_ = conn.Reply(ctx, req.ID, result)
	}
}

var errNoAnswer = errors.New("this request is left unanswered")

func (s *stub) answer(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (any, error) {
	switch req.Method {
	case acp.MethodInitialize:
		return s.initialize(), nil
	case acp.MethodNewSession:
		var p acp.NewSessionParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return newSession(p.Cwd)
	case acp.MethodListSessions:
		var p acp.ListSessionsParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return listSessions(p.Cwd)
	case acp.MethodResumeSession:
		var p acp.ResumeSessionParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return struct{}{}, s.resume(ctx, conn, p)
	case acp.MethodPrompt:
		var p acp.PromptParams
		if err := unmarshal(req, &p); err != nil {
			return nil, err
		}
		return s.prompt(ctx, conn, p)
	default:
		return nil, fmt.Errorf("%s is not implemented", req.Method)
	}
}

func (s *stub) initialize() acp.InitializeResult {
	result := acp.InitializeResult{
		ProtocolVersion: acp.ProtocolVersion,
		AgentInfo:       &acp.Implementation{Name: "stubagent"},
	}
	if !s.noSessionCapabilities {
		result.AgentCapabilities.SessionCapabilities = acp.SessionCapabilities{
			List:   &acp.Capability{},
			Resume: &acp.Capability{},
		}
	}
	return result
}

func (s *stub) resume(ctx context.Context, conn *jsonrpc2.Conn, p acp.ResumeSessionParams) error {
	if err := absolute(p.Cwd); err != nil {
		return err
	}
	stored, err := os.ReadFile(sessionFile(p.SessionID))
	if err != nil {
		return err
	}
	if !s.streamOnResume {
		return nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(stored)), "\n") {
		if line == "" {
			continue
		}
		if err := chunk(ctx, conn, p.SessionID, line); err != nil {
			return err
		}
	}
	return nil
}

func (s *stub) prompt(ctx context.Context, conn *jsonrpc2.Conn, p acp.PromptParams) (any, error) {
	if s.neverAnswerPrompt {
		<-ctx.Done()
		return nil, errNoAnswer
	}
	if s.failPrompt {
		return nil, errors.New("the harness refuses to answer this prompt")
	}
	text := core.TextOf(p.Prompt)
	if err := appendPrompt(p.SessionID, text); err != nil {
		return nil, err
	}
	if err := chunk(ctx, conn, p.SessionID, p.SessionID); err != nil {
		return nil, err
	}
	if err := chunk(ctx, conn, p.SessionID, text); err != nil {
		return nil, err
	}
	if s.askPermission {
		selected, err := requestPermission(ctx, conn, p.SessionID)
		if err != nil {
			return nil, err
		}
		if err := chunk(ctx, conn, p.SessionID, "selected="+selected); err != nil {
			return nil, err
		}
	}
	return acp.PromptResult{StopReason: acp.StopReasonEndTurn}, nil
}

// requestPermission offers a rejecting option before the allowing ones, so a
// client that takes the first option of the list is not taken for a client that
// reads the kinds.
func requestPermission(ctx context.Context, conn *jsonrpc2.Conn, sessionID string) (string, error) {
	var result acp.RequestPermissionResult
	err := conn.Call(ctx, acp.MethodRequestPermission, acp.RequestPermissionParams{
		SessionID: sessionID,
		Options: []acp.PermissionOption{
			{OptionID: "reject-once", Name: "Reject", Kind: "reject_once"},
			{OptionID: "allow-once", Name: "Allow once", Kind: acp.AllowOnce},
			{OptionID: "allow-always", Name: "Always allow", Kind: acp.AllowAlways},
		},
	}, &result)
	if err != nil {
		return "", err
	}
	if result.Outcome.Outcome != acp.OutcomeSelected {
		return "", fmt.Errorf("the client answered %q", result.Outcome.Outcome)
	}
	return result.Outcome.OptionID, nil
}

func chunk(ctx context.Context, conn *jsonrpc2.Conn, sessionID, text string) error {
	return conn.Notify(ctx, acp.MethodUpdate, acp.SessionNotification{
		SessionID: sessionID,
		Update: acp.SessionUpdate{
			SessionUpdate: acp.AgentMessageChunk,
			Content:       core.Text(text)[0],
		},
	})
}

// The stub owns its storage, as an agent does: one file per session in the
// working directory it runs in, holding the prompts of that session. The client
// starts it in the directory it names as cwd, so the two are the same place.

func sessionFile(sessionID string) string { return sessionID + ".session" }

func absolute(cwd string) error {
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd %q is not absolute", cwd)
	}
	return nil
}

func newSession(cwd string) (acp.NewSessionResult, error) {
	if err := absolute(cwd); err != nil {
		return acp.NewSessionResult{}, err
	}
	id := rand.Text()
	return acp.NewSessionResult{SessionID: id}, os.WriteFile(sessionFile(id), nil, 0o600)
}

func appendPrompt(sessionID, text string) error {
	f, err := os.OpenFile(sessionFile(sessionID), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, text); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func listSessions(cwd string) (acp.ListSessionsResult, error) {
	if err := absolute(cwd); err != nil {
		return acp.ListSessionsResult{}, err
	}
	files, err := filepath.Glob("*.session")
	if err != nil {
		return acp.ListSessionsResult{}, err
	}
	slices.Sort(files)
	result := acp.ListSessionsResult{Sessions: []acp.SessionInfo{}}
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".session")
		result.Sessions = append(result.Sessions, acp.SessionInfo{SessionID: id})
	}
	return result, nil
}

func unmarshal(req *jsonrpc2.Request, v any) error {
	if req.Params == nil {
		return errors.New("params are required")
	}
	return json.Unmarshal(*req.Params, v)
}
