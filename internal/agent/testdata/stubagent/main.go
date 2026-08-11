// Command stubagent is a minimal ACP agent for momo's harness tests, written
// against the specification: enough to prove the handshake, session creation,
// listing and resuming, a prompt answered with streamed content, and a
// permission request. It keeps its one session in a file in the session's
// working directory, which is what makes the session findable again.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sessions turns the session listing and resuming capabilities off, so a client
// facing an agent that has neither can be tested.
var sessions = flag.Bool("sessions", true, "advertise session listing and resuming")

type request struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params struct {
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	} `json:"params"`
}

type permissionResponse struct {
	Result struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	} `json:"result"`
}

func main() {
	flag.Parse()
	// The pid file lets a test see that the process this turn spawned is gone.
	write("stubagent.pid", fmt.Sprint(os.Getpid()))
	in, out := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var req request
		if err := in.Decode(&req); err != nil {
			return
		}
		result := handle(in, out, req)
		if req.ID == nil {
			continue
		}
		send(out, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

func handle(in *json.Decoder, out *json.Encoder, req request) any {
	switch req.Method {
	case "initialize":
		return initialize()
	case "session/new":
		id := "session-" + random()
		write(filepath.Join(req.Params.Cwd, "stubagent.session"), id)
		return map[string]any{"sessionId": id}
	case "session/list":
		return list(req.Params.Cwd)
	case "session/resume":
		// A resumed session says something of its own before the turn begins: a
		// client accumulating from the spawn would deliver this as the turn's reply.
		update(out, req.Params.SessionID, "stale content from an earlier turn")
		return map[string]any{}
	case "session/prompt":
		return prompt(in, out, req)
	default:
		fmt.Fprintf(os.Stderr, "stubagent: %s is not implemented\n", req.Method)
		return map[string]any{}
	}
}

func initialize() any {
	capabilities := map[string]any{"promptCapabilities": map[string]any{"image": true}}
	if *sessions {
		capabilities["sessionCapabilities"] = map[string]any{
			"list":   map[string]any{},
			"resume": map[string]any{},
		}
	}
	return map[string]any{
		"protocolVersion":   1,
		"agentCapabilities": capabilities,
		"agentInfo":         map[string]any{"name": "stubagent"},
	}
}

func list(cwd string) any {
	id, err := os.ReadFile(filepath.Join(cwd, "stubagent.session"))
	if err != nil {
		return map[string]any{"sessions": []any{}}
	}
	return map[string]any{"sessions": []any{map[string]any{"sessionId": string(id), "cwd": cwd}}}
}

// prompt answers with two content blocks, so a client has to accumulate the
// stream, and names the session it ran in, so a client's session handling is
// visible in the reply.
func prompt(in *json.Decoder, out *json.Encoder, req request) any {
	var text strings.Builder
	for _, block := range req.Params.Prompt {
		text.WriteString(block.Text)
	}
	answer := "reply to " + text.String()
	if strings.Contains(text.String(), "act") {
		answer += " permission=" + permit(in, out, req.Params.SessionID)
	}
	update(out, req.Params.SessionID, answer)
	update(out, req.Params.SessionID, "session="+req.Params.SessionID)
	return map[string]any{"stopReason": "end_turn"}
}

// permit asks the client to approve a tool call. The rejecting option comes
// first, so a client that picks by position rather than by kind is visible.
func permit(in *json.Decoder, out *json.Encoder, sessionID string) string {
	send(out, map[string]any{
		"jsonrpc": "2.0",
		"id":      9001,
		"method":  "session/request_permission",
		"params": map[string]any{
			"sessionId": sessionID,
			"toolCall":  map[string]any{"toolCallId": "call-1", "title": "run a command"},
			"options": []any{
				map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"},
				map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
			},
		},
	})
	var resp permissionResponse
	if err := in.Decode(&resp); err != nil {
		return "unanswered"
	}
	if resp.Result.Outcome.Outcome != "selected" {
		return resp.Result.Outcome.Outcome
	}
	return resp.Result.Outcome.OptionID
}

func update(out *json.Encoder, sessionID, text string) {
	send(out, map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
			},
		},
	})
}

func send(out *json.Encoder, message any) {
	if err := out.Encode(message); err != nil {
		fmt.Fprintf(os.Stderr, "stubagent: writing: %v\n", err)
		os.Exit(1)
	}
}

func write(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "stubagent: writing %s: %v\n", path, err)
		os.Exit(1)
	}
}

func random() string {
	b := make([]byte, 8)
	// crypto/rand.Read never fails as of Go 1.24.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
