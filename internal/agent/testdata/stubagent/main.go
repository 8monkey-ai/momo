// Command stubagent is an ACP v1 agent for the agent package's tests. It speaks
// newline-delimited JSON-RPC 2.0 on stdin and stdout, answers initialize,
// session/new and session/prompt, and exits on EOF.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
)

// chunks is the reply of every prompt, one content block for each element, so a
// test asserts a literal.
var chunks = []string{"hello from", "the stub agent"}

type request struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
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
		if err := answer(enc, req); err != nil {
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

func answer(enc *json.Encoder, req request) error {
	switch req.Method {
	case "initialize":
		return respond(enc, req.ID, map[string]any{"protocolVersion": 1})
	case "session/new":
		if err := reportCwd(req.Params); err != nil {
			return err
		}
		return respond(enc, req.ID, map[string]any{"sessionId": "stub-session"})
	case "session/prompt":
		return prompt(enc, req)
	default:
		return respondError(enc, req.ID, req.Method+" is not implemented")
	}
}

// reportCwd tells the test which working directory momo asked the session for.
func reportCwd(params json.RawMessage) error {
	path := os.Getenv("STUBAGENT_CWD_FILE")
	if path == "" {
		return nil
	}
	var p struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(p.Cwd), 0o600)
}

func prompt(enc *json.Encoder, req request) error {
	if err := synchronise(); err != nil {
		return err
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
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
	return respond(enc, req.ID, map[string]any{"stopReason": "end_turn"})
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
