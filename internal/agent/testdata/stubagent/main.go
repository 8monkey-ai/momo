// Command stubagent is an ACP v1 agent for the agent package's tests. It speaks
// newline-delimited JSON-RPC 2.0 on stdin and stdout, answers initialize,
// session/new, session/list, session/resume and session/prompt, asks for the
// permission a test asks it to ask for, and exits on EOF.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

// chunks is the reply of every prompt, one content block for each element, so a
// test asserts a literal. The two of them are one blank line apart, so the reply
// is one message or two paragraphs, depending on the delivery under test.
var chunks = []string{"hello from\n\n", "the stub agent"}

type request struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
}

// params holds every member of a session method the stub reads.
type params struct {
	Cwd       string `json:"cwd"`
	SessionID string `json:"sessionId"`
}

func main() {
	if err := reportPID(); err != nil {
		fmt.Fprintln(os.Stderr, "stubagent:", err)
		os.Exit(1)
	}
	dec, enc := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			return
		}
		if err := answer(dec, enc, req); err != nil {
			fmt.Fprintln(os.Stderr, "stubagent:", err)
			return
		}
	}
}

// reportPID tells the test which process to look for after the turn.
func reportPID() error {
	path := os.Getenv("STUBAGENT_PID_FILE")
	if path == "" {
		return nil
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

func answer(dec *json.Decoder, enc *json.Encoder, req request) error {
	switch req.Method {
	case "initialize":
		return respond(enc, req.ID, map[string]any{
			"protocolVersion":   1,
			"agentCapabilities": map[string]any{"sessionCapabilities": sessionCapabilities()},
		})
	case "session/new":
		return newSession(enc, req)
	case "session/list":
		return listSessions(enc, req)
	case "session/resume":
		return resumeSession(enc, req)
	case "session/prompt":
		return prompt(dec, enc, req)
	default:
		return respondError(enc, req.ID, req.Method+" is not implemented")
	}
}

// sessionCapabilities advertises what the test asks of this run: a capability is
// an object when supported and absent when not.
func sessionCapabilities() map[string]any {
	if os.Getenv("STUBAGENT_NO_SESSION_CAPS") != "" {
		return map[string]any{}
	}
	return map[string]any{"list": map[string]any{}, "resume": map[string]any{}}
}

func newSession(enc *json.Encoder, req request) error {
	var p params
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return err
	}
	if err := reportCwd(p.Cwd); err != nil {
		return err
	}
	_, total, err := created(p.Cwd)
	if err != nil {
		return err
	}
	sessionID := "stub-session-" + strconv.Itoa(total)
	if err := trace("session/new", p.Cwd, sessionID); err != nil {
		return err
	}
	return respond(enc, req.ID, map[string]any{"sessionId": sessionID})
}

func listSessions(enc *json.Encoder, req request) error {
	var p params
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return err
	}
	ids, _, err := created(p.Cwd)
	if err != nil {
		return err
	}
	if err := trace("session/list", p.Cwd); err != nil {
		return err
	}
	sessions := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		sessions = append(sessions, map[string]any{"sessionId": id, "cwd": p.Cwd})
	}
	return respond(enc, req.ID, map[string]any{"sessions": sessions})
}

func resumeSession(enc *json.Encoder, req request) error {
	var p params
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return err
	}
	if err := trace("session/resume", p.SessionID); err != nil {
		return err
	}
	return respond(enc, req.ID, map[string]any{})
}

// reportCwd tells the test which working directory momo asked the session for.
func reportCwd(cwd string) error {
	path := os.Getenv("STUBAGENT_CWD_FILE")
	if path == "" {
		return nil
	}
	return os.WriteFile(path, []byte(cwd), 0o600)
}

func traceFile() string { return os.Getenv("STUBAGENT_TRACE") }

// trace appends one tab separated line for the method handled. The file is the
// stub's session store as well: momo starts a process for each turn, so a session
// outlives its process only in a file, and the file lies outside the conversation
// directory, which stays empty.
func trace(fields ...string) error {
	path := traceFile()
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = fmt.Fprintln(f, strings.Join(fields, "\t"))
	return err
}

// created answers the sessions of cwd, in creation order, and how many sessions
// the store holds altogether, which names the next one.
func created(cwd string) ([]string, int, error) {
	path := traceFile()
	if path == "" {
		return nil, 0, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var ids []string
	total := 0
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] != "session/new" {
			continue
		}
		total++
		if fields[1] == cwd {
			ids = append(ids, fields[2])
		}
	}
	return ids, total, nil
}

func prompt(dec *json.Decoder, enc *json.Encoder, req request) error {
	if err := synchronise(); err != nil {
		return err
	}
	var p params
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return err
	}
	if err := requestPermission(dec, enc, p.SessionID); err != nil {
		return err
	}
	for _, text := range chunks {
		update := map[string]any{
			"sessionId": p.SessionID,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
			},
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": update}); err != nil {
			return err
		}
	}
	if err := respond(enc, req.ID, map[string]any{"stopReason": "end_turn"}); err != nil {
		return err
	}
	return lateChunk(enc, p.SessionID)
}

// lateChunk streams one chunk after the turn was answered, which v1 does not
// allow, so a test drives what momo does with content that belongs to no turn.
func lateChunk(enc *json.Encoder, sessionID string) error {
	if os.Getenv("STUBAGENT_LATE_CHUNK") == "" {
		return nil
	}
	update := map[string]any{
		"sessionId": sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "after the turn"},
		},
	}
	return enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": update})
}

// requestPermission asks the client, in the middle of the turn, to allow one tool
// call, and records the outcome the client answered with. The refusing option
// comes first, so a client that selects the first option of the list is told
// apart from one that selects an allowing option.
func requestPermission(dec *json.Decoder, enc *json.Encoder, sessionID string) error {
	offered := os.Getenv("STUBAGENT_PERMISSION")
	if offered == "" {
		return nil
	}
	options := []map[string]any{{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"}}
	if offered != "refusals" {
		options = append(options,
			map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
			map[string]any{"optionId": "allow-always", "name": "Allow always", "kind": "allow_always"},
		)
	}
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "session/request_permission",
		"params": map[string]any{
			"sessionId": sessionID,
			"toolCall":  map[string]any{"toolCallId": "stub-call"},
			"options":   options,
		},
	}); err != nil {
		return err
	}
	var answer struct {
		Result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := dec.Decode(&answer); err != nil {
		return err
	}
	return trace("session/request_permission", answer.Result.Outcome.Outcome, answer.Result.Outcome.OptionID)
}

// synchronise holds the prompt until the test releases it: the stub dials the
// address, writes one byte, and waits for one byte back. A test that accepts
// every connection before it answers any of them proves that the prompts run at
// the same time.
func synchronise() error {
	addr := os.Getenv("STUBAGENT_SYNC_ADDR")
	if addr == "" {
		return nil
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte{'.'}); err != nil {
		return err
	}
	_, err = io.ReadFull(conn, make([]byte, 1))
	return err
}

func respond(enc *json.Encoder, id *json.RawMessage, result any) error {
	return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func respondError(enc *json.Encoder, id *json.RawMessage, message string) error {
	return enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32601, "message": message},
	})
}
