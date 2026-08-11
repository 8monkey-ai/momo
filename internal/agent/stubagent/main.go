// Command stubagent is a minimal ACP v1 agent for the agent package's tests. It
// proves the client side of the handshake, session creation and resumption,
// prompting with a streamed reply, and a permission request; nothing else. Its
// sessions are recorded in the working directory it is started in, so a later
// process finds the session a former one created.
//
// It answers a prompt with the session it is running and the text it was given,
// so a test reads the session identity off the reply. A prompt containing
// "permission" asks for permission first and reports the option that was chosen.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sourcegraph/jsonrpc2"
)

const sessionsFile = "sessions.json"

func main() {
	resume := flag.Bool("resume", true, "advertise session listing and loading")
	flag.Parse()

	if err := os.WriteFile("stubagent.pid", []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "stubagent:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "stubagent started")

	a := &agent{resume: *resume}
	// A prompt calls back to the client and waits for the answer, so the read
	// loop must stay free while a handler runs.
	a.conn = jsonrpc2.NewConn(context.Background(), jsonrpc2.NewPlainObjectStream(stdio{}), jsonrpc2.AsyncHandler(a))
	<-a.conn.DisconnectNotify()
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return os.Stdin.Close() }

type agent struct {
	conn   *jsonrpc2.Conn
	resume bool
	mu     sync.Mutex
}

type sessionRecord struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	UpdatedAt string `json:"updatedAt"`
}

func (a *agent) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	result, err := a.answer(ctx, req)
	if req.Notif {
		return
	}
	if err != nil {
		_ = conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{Code: jsonrpc2.CodeInternalError, Message: err.Error()})
		return
	}
	_ = conn.Reply(ctx, req.ID, result)
}

func (a *agent) answer(ctx context.Context, req *jsonrpc2.Request) (any, error) {
	switch req.Method {
	case "initialize":
		list := json.RawMessage("null")
		if a.resume {
			list = json.RawMessage("{}")
		}
		return map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession":         a.resume,
				"sessionCapabilities": map[string]any{"list": list},
			},
		}, nil
	case "session/new":
		return a.newSession()
	case "session/list":
		return map[string]any{"sessions": a.sessions()}, nil
	case "session/load":
		return a.loadSession(req)
	case "session/prompt":
		return a.prompt(ctx, req)
	}
	return nil, fmt.Errorf("%s is not implemented", req.Method)
}

func (a *agent) newSession() (any, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("stub-%d", time.Now().UnixNano())
	a.mu.Lock()
	defer a.mu.Unlock()
	records := append(a.read(), sessionRecord{SessionID: id, Cwd: cwd, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err := a.write(records); err != nil {
		return nil, err
	}
	return map[string]any{"sessionId": id}, nil
}

func (a *agent) loadSession(req *jsonrpc2.Request) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := unmarshal(req, &p); err != nil {
		return nil, err
	}
	for _, r := range a.sessions() {
		if r.SessionID == p.SessionID {
			return map[string]any{}, nil
		}
	}
	return nil, fmt.Errorf("unknown session %q", p.SessionID)
}

func (a *agent) prompt(ctx context.Context, req *jsonrpc2.Request) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := unmarshal(req, &p); err != nil {
		return nil, err
	}
	var text []string
	for _, block := range p.Prompt {
		text = append(text, block.Text)
	}
	said := strings.Join(text, " ")

	if strings.Contains(said, "permission") {
		chosen, err := a.askPermission(ctx, p.SessionID)
		if err != nil {
			return nil, err
		}
		if err := a.chunk(ctx, p.SessionID, "permission:"+chosen); err != nil {
			return nil, err
		}
	}
	if err := a.chunk(ctx, p.SessionID, "session="+p.SessionID); err != nil {
		return nil, err
	}
	if err := a.chunk(ctx, p.SessionID, "echo:"+said); err != nil {
		return nil, err
	}
	return map[string]any{"stopReason": "end_turn"}, nil
}

// askPermission offers a rejection first, so a client that takes the first
// option on offer fails the test that exercises this.
func (a *agent) askPermission(ctx context.Context, sessionID string) (string, error) {
	params := map[string]any{
		"sessionId": sessionID,
		"toolCall":  map[string]any{"toolCallId": "call-1"},
		"options": []map[string]any{
			{"optionId": "no", "name": "Reject", "kind": "reject_once"},
			{"optionId": "yes", "name": "Allow", "kind": "allow_always"},
		},
	}
	var res struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := a.conn.Call(ctx, "session/request_permission", params, &res); err != nil {
		return "", err
	}
	if res.Outcome.Outcome != "selected" {
		return res.Outcome.Outcome, nil
	}
	return res.Outcome.OptionID, nil
}

func (a *agent) chunk(ctx context.Context, sessionID, text string) error {
	return a.conn.Notify(ctx, "session/update", map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": text},
		},
	})
}

func (a *agent) sessions() []sessionRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.read()
}

func (a *agent) read() []sessionRecord {
	raw, err := os.ReadFile(sessionsFile)
	if err != nil {
		return nil
	}
	var records []sessionRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil
	}
	return records
}

func (a *agent) write(records []sessionRecord) error {
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Clean(sessionsFile), raw, 0o600)
}

func unmarshal(req *jsonrpc2.Request, v any) error {
	if req.Params == nil {
		return fmt.Errorf("%s: params are required", req.Method)
	}
	return json.Unmarshal(*req.Params, v)
}
