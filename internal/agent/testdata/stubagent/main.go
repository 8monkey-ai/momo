// Command stubagent is a small ACP v1 agent for the tests of the agent package.
// It is not part of momo: the tests build it, and the harness command points to
// it.
//
// It writes the wire by hand, from the specification, so a mistake in momo's own
// ACP vocabulary shows up as a failing test. STUB_BEHAVIOUR selects what the stub
// shows: an empty value is the full flow, "no-sessions" advertises neither
// listing nor resumption, "permission" asks before it answers, "fail" answers the
// prompt with an error, and "hang" never answers.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// stateFile is what the stub stores in its working directory. The agent owns all
// storage, so the session outlives the process and momo keeps nothing.
const (
	stateFile = "stub-session.json"
	pidFile   = "stub.pid"
)

type state struct {
	SessionID string `json:"sessionId"`
	Turns     int    `json:"turns"`
}

type request struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
}

type stub struct {
	behaviour string
	in        *bufio.Scanner
	out       *json.Encoder
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "stubagent:", err)
		os.Exit(1)
	}
}

func run() error {
	s := &stub{
		behaviour: os.Getenv("STUB_BEHAVIOUR"),
		in:        bufio.NewScanner(os.Stdin),
		out:       json.NewEncoder(os.Stdout),
	}
	// The pid lets a test see that momo stopped the subprocess.
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "stubagent ready")
	for {
		req, ok, err := s.next()
		if err != nil || !ok {
			return err
		}
		if err := s.handle(req); err != nil {
			return err
		}
	}
}

func (s *stub) next() (request, bool, error) {
	if !s.in.Scan() {
		return request{}, false, s.in.Err()
	}
	var req request
	if err := json.Unmarshal(s.in.Bytes(), &req); err != nil {
		return request{}, false, err
	}
	return req, true, nil
}

func (s *stub) handle(req request) error {
	switch req.Method {
	case "initialize":
		return s.result(req.ID, map[string]any{
			"protocolVersion":   1,
			"agentInfo":         map[string]any{"name": "stubagent"},
			"agentCapabilities": map[string]any{"sessionCapabilities": s.sessionCapabilities()},
		})
	case "session/list":
		return s.list(req)
	case "session/resume":
		return s.resume(req)
	case "session/new":
		return s.newSession(req)
	case "session/prompt":
		return s.prompt(req)
	default:
		return s.fail(req.ID, req.Method+" is not implemented")
	}
}

func (s *stub) sessionCapabilities() map[string]any {
	if s.behaviour == "no-sessions" {
		return map[string]any{}
	}
	return map[string]any{"list": map[string]any{}, "resume": map[string]any{}}
}

func (s *stub) list(req request) error {
	stored, err := load()
	if err != nil {
		return err
	}
	sessions := []any{}
	if stored.SessionID != "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		sessions = append(sessions, map[string]any{"sessionId": stored.SessionID, "cwd": cwd})
	}
	return s.result(req.ID, map[string]any{"sessions": sessions})
}

func (s *stub) resume(req request) error {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return err
	}
	stored, err := load()
	if err != nil {
		return err
	}
	if p.SessionID != stored.SessionID {
		return s.fail(req.ID, "session "+p.SessionID+" is not stored here")
	}
	return s.result(req.ID, map[string]any{})
}

func (s *stub) newSession(req request) error {
	stored := state{SessionID: "stub-" + strconv.FormatInt(time.Now().UnixNano(), 36)}
	if err := save(stored); err != nil {
		return err
	}
	return s.result(req.ID, map[string]any{"sessionId": stored.SessionID})
}

func (s *stub) prompt(req request) error {
	switch s.behaviour {
	case "hang":
		// The turn never ends: momo's turn timeout is what releases it.
		time.Sleep(time.Hour)
		return nil
	case "fail":
		return s.fail(req.ID, "the stub was told to fail this turn")
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return err
	}
	stored, err := load()
	if err != nil {
		return err
	}
	stored.Turns++
	if err := save(stored); err != nil {
		return err
	}
	if s.behaviour == "filesystem" {
		refusal, err := s.readTextFile(p.SessionID)
		if err != nil {
			return err
		}
		if err := s.chunk(p.SessionID, "filesystem "+refusal+" "); err != nil {
			return err
		}
	}
	if s.behaviour == "permission" {
		selected, err := s.permission(p.SessionID)
		if err != nil {
			return err
		}
		if err := s.chunk(p.SessionID, "permission "+selected+" "); err != nil {
			return err
		}
	}
	// The reply arrives in two chunks, the way an agent streams a message.
	if err := s.chunk(p.SessionID, fmt.Sprintf("turn %d:", stored.Turns)); err != nil {
		return err
	}
	for _, block := range p.Prompt {
		if err := s.chunk(p.SessionID, block.Text); err != nil {
			return err
		}
	}
	return s.result(req.ID, map[string]any{"stopReason": "end_turn"})
}

// call sends a request to the client and returns the answer as it arrived.
func (s *stub) call(id, method string, params map[string]any) ([]byte, error) {
	err := s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if !s.in.Scan() {
		return nil, errors.Join(s.in.Err(), errors.New("the client did not answer "+method))
	}
	return s.in.Bytes(), nil
}

// permission asks the client to allow a tool call and returns the option the
// client selected.
func (s *stub) permission(session string) (string, error) {
	answer, err := s.call("permission-1", "session/request_permission", map[string]any{
		"sessionId": session,
		"toolCall":  map[string]any{"toolCallId": "call-1", "title": "read a file"},
		"options": []any{
			map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
			map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
		},
	})
	if err != nil {
		return "", err
	}
	var res struct {
		Result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(answer, &res); err != nil {
		return "", err
	}
	if res.Result.Outcome.Outcome != "selected" {
		return res.Result.Outcome.Outcome, nil
	}
	return res.Result.Outcome.OptionID, nil
}

// readTextFile asks the client to read a file, which momo does not offer, and
// reports the code the client refused with.
func (s *stub) readTextFile(session string) (string, error) {
	answer, err := s.call("read-1", "fs/read_text_file", map[string]any{
		"sessionId": session,
		"path":      "/etc/hosts",
	})
	if err != nil {
		return "", err
	}
	var res struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answer, &res); err != nil {
		return "", err
	}
	if res.Error == nil {
		return "answered", nil
	}
	return strconv.Itoa(res.Error.Code), nil
}

func (s *stub) chunk(session, text string) error {
	return s.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": session,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
			},
		},
	})
}

func (s *stub) result(id *json.RawMessage, result any) error {
	return s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *stub) fail(id *json.RawMessage, message string) error {
	return s.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32000, "message": message},
	})
}

func (s *stub) write(message any) error { return s.out.Encode(message) }

func load() (state, error) {
	raw, err := os.ReadFile(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	var stored state
	err = json.Unmarshal(raw, &stored)
	return stored, err
}

func save(stored state) error {
	raw, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, raw, 0o600)
}
