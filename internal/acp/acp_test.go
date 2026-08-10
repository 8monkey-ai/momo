package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

const token = "test-token"

type capture struct {
	messages chan core.Message
}

func (c capture) Received(_ context.Context, m core.Message) { c.messages <- m }
func (c capture) Sent(_ context.Context, m core.Message)     { c.messages <- m }

type harness struct {
	t        *testing.T
	url      string
	channel  *acp
	messages chan core.Message
	server   *http.Server
}

func start(t *testing.T) *harness {
	t.Helper()
	messages := make(chan core.Message, 4)
	built, err := New(func(v any) error {
		s, ok := v.(*settings)
		if !ok {
			t.Fatalf("decoded into %T, want *settings", v)
		}
		s.Token = token
		return nil
	}, capture{messages: messages})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a, ok := built.(*acp)
	if !ok {
		t.Fatalf("New built %T, want *acp", built)
	}
	route := a.Routes()[0]
	mux := http.NewServeMux()
	mux.Handle(route.Path, route.Handler)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		a.conns.stop()
		_ = server.Close()
	})
	return &harness{
		t:        t,
		url:      "http://" + listener.Addr().String() + route.Path,
		channel:  a,
		messages: messages,
		server:   server,
	}
}

// connections counts the live connections, so a refused request can be shown to
// have left no state behind.
func (h *harness) connections() int {
	h.channel.conns.mu.Lock()
	defer h.channel.conns.mu.Unlock()
	return len(h.channel.conns.conns)
}

func (h *harness) do(method, body string, header http.Header) *http.Response {
	h.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	h.t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, method, h.url, reader)
	if err != nil {
		h.t.Fatalf("building the request: %v", err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s: %v", method, err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func authorized(identity ...string) http.Header {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	if len(identity) > 0 && identity[0] != "" {
		header.Set(connectionHeader, identity[0])
	}
	if len(identity) > 1 && identity[1] != "" {
		header.Set(sessionHeader, identity[1])
	}
	return header
}

func (h *harness) post(body string, identity ...string) *http.Response {
	h.t.Helper()
	header := authorized(identity...)
	header.Set("Content-Type", "application/json")
	return h.do(http.MethodPost, body, header)
}

func (h *harness) get(identity ...string) *http.Response {
	h.t.Helper()
	header := authorized(identity...)
	header.Set("Accept", "text/event-stream")
	return h.do(http.MethodGet, "", header)
}

// answered is a JSON-RPC answer as it arrives on a stream.
type answered struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (h *harness) initialize() string {
	h.t.Helper()
	resp := h.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("initialize status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var got answered
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		h.t.Fatalf("reading the initialize response: %v", err)
	}
	var result initializeResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		h.t.Fatalf("reading the initialize result: %v", err)
	}
	if result.ProtocolVersion != 1 {
		h.t.Fatalf("protocolVersion = %d, want 1", result.ProtocolVersion)
	}
	if result.ConnectionID == "" || resp.Header.Get(connectionHeader) != result.ConnectionID {
		h.t.Fatalf("connection id %q in the body, %q in the header", result.ConnectionID, resp.Header.Get(connectionHeader))
	}
	return result.ConnectionID
}

// stream opens an SSE stream and returns a reader over the answers on it.
func (h *harness) stream(identity ...string) *bufio.Reader {
	h.t.Helper()
	resp := h.get(identity...)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("stream status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	return bufio.NewReader(resp.Body)
}

// drain reads to the end of a stream, reporting why it ended.
func drain(r *bufio.Reader) error {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) != "" {
			return fmt.Errorf("unexpected message %q", line)
		}
	}
}

func next(t *testing.T, r *bufio.Reader) answered {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data: ")
		if !ok {
			continue
		}
		var got answered
		if err := json.Unmarshal([]byte(payload), &got); err != nil {
			t.Fatalf("stream carried %q, which is not a JSON-RPC message: %v", payload, err)
		}
		return got
	}
}

// newSession creates a session and reads its id off the connection-scoped
// stream, where the RFD puts the answer.
func (h *harness) newSession(connID string, connStream *bufio.Reader) string {
	h.t.Helper()
	resp := h.post(`{"jsonrpc":"2.0","id":2,"method":"session/new",`+
		`"params":{"cwd":"/workspace","mcpServers":[]}}`, connID)
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("session/new status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	var result newSessionResult
	if err := json.Unmarshal(next(h.t, connStream).Result, &result); err != nil {
		h.t.Fatalf("reading the session/new result: %v", err)
	}
	if result.SessionID == "" {
		h.t.Fatal("session/new answered without a session id")
	}
	return result.SessionID
}

func TestTokenRequiredOnEveryMethod(t *testing.T) {
	h := start(t)
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		for _, presented := range []string{"", "Bearer wrong-token", token, "Bearer "} {
			header := http.Header{
				"Content-Type": {"application/json"},
				"Accept":       {"text/event-stream"},
			}
			if presented != "" {
				header.Set("Authorization", presented)
			}
			resp := h.do(method, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, header)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s with %q: status = %d, want %d", method, presented, resp.StatusCode, http.StatusUnauthorized)
			}
		}
	}
	if got := h.connections(); got != 0 {
		t.Fatalf("%d connections exist, want none: a refused request must create nothing", got)
	}
}

func TestContentTypeAndAcceptRejections(t *testing.T) {
	h := start(t)
	for _, tc := range []struct {
		name        string
		contentType string
	}{
		{name: "missing"},
		{name: "not json", contentType: "text/plain"},
		{name: "form", contentType: "application/x-www-form-urlencoded"},
	} {
		t.Run("content type "+tc.name, func(t *testing.T) {
			header := authorized()
			if tc.contentType != "" {
				header.Set("Content-Type", tc.contentType)
			}
			resp := h.do(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, header)
			if resp.StatusCode != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnsupportedMediaType)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		accept string
	}{
		{name: "missing"},
		{name: "json only", accept: "application/json"},
	} {
		t.Run("accept "+tc.name, func(t *testing.T) {
			header := authorized(h.initialize())
			if tc.accept != "" {
				header.Set("Accept", tc.accept)
			}
			resp := h.do(http.MethodGet, "", header)
			if resp.StatusCode != http.StatusNotAcceptable {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotAcceptable)
			}
		})
	}
}

func TestIdentityHeaders(t *testing.T) {
	h := start(t)
	connID := h.initialize()
	connStream := h.stream(connID)
	sessionID := h.newSession(connID, connStream)
	other := h.initialize()

	prompt := `{"jsonrpc":"2.0","id":3,"method":"session/prompt",` +
		`"params":{"prompt":[{"type":"text","text":"hello"}]}}`
	newSession := `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}`

	t.Run("post without a connection id", func(t *testing.T) {
		if got := h.post(newSession).StatusCode; got != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
		}
	})
	t.Run("post with an unknown connection id", func(t *testing.T) {
		if got := h.post(newSession, "no-such-connection").StatusCode; got != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", got, http.StatusNotFound)
		}
	})
	t.Run("session-scoped post without a session id", func(t *testing.T) {
		if got := h.post(prompt, connID).StatusCode; got != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
		}
	})
	t.Run("session id from another connection", func(t *testing.T) {
		if got := h.post(prompt, other, sessionID).StatusCode; got != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", got, http.StatusNotFound)
		}
	})
	t.Run("stream without a connection id", func(t *testing.T) {
		if got := h.get().StatusCode; got != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
		}
	})
	t.Run("stream with an unknown session id", func(t *testing.T) {
		if got := h.get(connID, "no-such-session").StatusCode; got != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", got, http.StatusNotFound)
		}
	})
	t.Run("delete without a connection id", func(t *testing.T) {
		if got := h.do(http.MethodDelete, "", authorized()).StatusCode; got != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
		}
	})
}

func TestBatchRefused(t *testing.T) {
	h := start(t)
	body := `[{"jsonrpc":"2.0","id":1,"method":"initialize"},{"jsonrpc":"2.0","id":2,"method":"session/new"}]`
	if got := h.post(body).StatusCode; got != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", got, http.StatusNotImplemented)
	}
	if got := h.connections(); got != 0 {
		t.Fatalf("%d connections exist, want none", got)
	}
}

func TestMalformedBody(t *testing.T) {
	h := start(t)
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "not json", body: "not json at all"},
		{name: "truncated", body: `{"jsonrpc":"2.0","id":1`},
		{name: "wrong field type", body: `{"jsonrpc":"2.0","id":1,"method":42}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.post(tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
			var got answered
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("reading the response: %v", err)
			}
			if got.Error == nil || got.Error.Code != -32700 {
				t.Fatalf("error = %+v, want the parse error code -32700", got.Error)
			}
		})
	}
}

func TestRequestWithoutAnID(t *testing.T) {
	h := start(t)
	connID := h.initialize()
	connStream := h.stream(connID)
	sessionID := h.newSession(connID, connStream)

	for _, tc := range []struct {
		name     string
		body     string
		identity []string
	}{
		{name: "initialize", body: `{"jsonrpc":"2.0","method":"initialize"}`},
		{name: "session/new", body: `{"jsonrpc":"2.0","method":"session/new"}`, identity: []string{connID}},
		{
			name:     "session/prompt",
			body:     `{"jsonrpc":"2.0","method":"session/prompt","params":{"prompt":[{"type":"text","text":"x"}]}}`,
			identity: []string{connID, sessionID},
		},
		{name: "null id", body: `{"jsonrpc":"2.0","id":null,"method":"session/new"}`, identity: []string{connID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.post(tc.body, tc.identity...)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
			var got answered
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("reading the response: %v", err)
			}
			if got.Error == nil || got.Error.Code != -32600 {
				t.Fatalf("error = %+v, want the invalid request code -32600", got.Error)
			}
		})
	}

	t.Run("session/cancel is a notification and needs none", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + sessionID + `"}}`
		if got := h.post(body, connID, sessionID).StatusCode; got != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", got, http.StatusAccepted)
		}
	})
}

func TestPromptReachesTheCoreAsSent(t *testing.T) {
	h := start(t)
	connID := h.initialize()
	connStream := h.stream(connID)
	sessionID := h.newSession(connID, connStream)
	sessionStream := h.stream(connID, sessionID)

	body := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"` + sessionID + `",` +
		`"prompt":[{"type":"text","text":"hello"},` +
		`{"type":"image","data":"aGk=","mimeType":"image/png"},` +
		`{"type":"resource_link","uri":"file:///notes.md","name":"notes.md"},` +
		`{"type":"audio","data":"YXVkaW8=","mimeType":"audio/wav"},` +
		`{"type":"resource","resource":{"uri":"file:///plan.md","text":"step one"}}]}}`
	if got := h.post(body, connID, sessionID).StatusCode; got != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", got, http.StatusAccepted)
	}

	want := core.Message{
		Contact: sessionID,
		Content: []core.ContentBlock{
			{Type: "text", Text: "hello"},
			{Type: "image", Data: "aGk=", MimeType: "image/png"},
			{Type: "resource_link", URI: "file:///notes.md", Name: "notes.md"},
			{Type: "audio", Data: "YXVkaW8=", MimeType: "audio/wav"},
			{Type: "resource", Resource: &core.EmbeddedResource{URI: "file:///plan.md", Text: "step one"}},
		},
	}
	select {
	case got := <-h.messages:
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("core received %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("the core was never called")
	}

	var result promptResult
	if err := json.Unmarshal(next(t, sessionStream).Result, &result); err != nil {
		t.Fatalf("reading the session/prompt result: %v", err)
	}
	if result.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want %q", result.StopReason, "end_turn")
	}
}

func TestPromptWithoutUsableContentIsRefused(t *testing.T) {
	h := start(t)
	connID := h.initialize()
	connStream := h.stream(connID)
	sessionID := h.newSession(connID, connStream)
	sessionStream := h.stream(connID, sessionID)

	for _, tc := range []struct {
		name   string
		params string
	}{
		{name: "no blocks", params: `{"prompt":[]}`},
		{name: "no prompt at all", params: `{}`},
		{name: "block without a type", params: `{"prompt":[{"text":"hello"}]}`},
		{name: "one block without a type", params: `{"prompt":[{"type":"text","text":"a"},{"text":"b"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":9,"method":"session/prompt","params":` + tc.params + `}`
			if got := h.post(body, connID, sessionID).StatusCode; got != http.StatusAccepted {
				t.Fatalf("status = %d, want %d", got, http.StatusAccepted)
			}
			got := next(t, sessionStream)
			if got.Error == nil || got.Error.Code != -32602 {
				t.Fatalf("answer = %+v, want the invalid params code -32602", got)
			}
			select {
			case m := <-h.messages:
				t.Fatalf("the core received %+v, want no message", m)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestUnimplementedMethodLeavesTheConnectionUsable(t *testing.T) {
	h := start(t)
	connID := h.initialize()
	connStream := h.stream(connID)

	body := `{"jsonrpc":"2.0","id":7,"method":"session/load","params":{"sessionId":"whatever"}}`
	if got := h.post(body, connID).StatusCode; got != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", got, http.StatusAccepted)
	}
	got := next(t, connStream)
	if got.Error == nil || got.Error.Code != -32601 {
		t.Fatalf("answer = %+v, want the method not found code -32601", got)
	}
	if id := string(got.ID); id != "7" {
		t.Fatalf("answer id = %s, want 7", id)
	}
	if session := h.newSession(connID, connStream); session == "" {
		t.Fatal("the connection is unusable after an unimplemented method")
	}
}

func TestAnswersLandOnTheirOwnStream(t *testing.T) {
	h := start(t)
	connID := h.initialize()
	connStream := h.stream(connID)
	sessionID := h.newSession(connID, connStream)
	sessionStream := h.stream(connID, sessionID)

	prompt := `{"jsonrpc":"2.0","id":5,"method":"session/prompt",` +
		`"params":{"prompt":[{"type":"text","text":"hello"}]}}`
	if got := h.post(prompt, connID, sessionID).StatusCode; got != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", got, http.StatusAccepted)
	}
	if id := string(next(t, sessionStream).ID); id != "5" {
		t.Fatalf("the session stream carried id %s, want the prompt's id 5", id)
	}
	// The next thing on the connection-scoped stream is the next session/new
	// answer, which shows the prompt answer did not land there too.
	if second := h.newSession(connID, connStream); second == sessionID {
		t.Fatal("session/new answered with the id of an existing session")
	}
}

func TestDeleteTerminatesTheConnection(t *testing.T) {
	h := start(t)
	connID := h.initialize()
	connStream := h.stream(connID)
	sessionID := h.newSession(connID, connStream)

	if got := h.do(http.MethodDelete, "", authorized(connID)).StatusCode; got != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", got, http.StatusNoContent)
	}
	if err := drain(connStream); !errors.Is(err, io.EOF) {
		t.Fatalf("the stream ended with %v, want EOF: terminating a connection must close its streams", err)
	}
	prompt := `{"jsonrpc":"2.0","id":6,"method":"session/prompt",` +
		`"params":{"prompt":[{"type":"text","text":"hello"}]}}`
	if got := h.post(prompt, connID, sessionID).StatusCode; got != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, the session outlived its connection", got, http.StatusNotFound)
	}
	if got := h.do(http.MethodDelete, "", authorized(connID)).StatusCode; got != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want %d", got, http.StatusNotFound)
	}
}

func TestWebsocketUpgradeIsRefused(t *testing.T) {
	h := start(t)
	header := authorized(h.initialize())
	header.Set("Accept", "text/event-stream")
	header.Set("Upgrade", "websocket")
	if got := h.do(http.MethodGet, "", header).StatusCode; got != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", got, http.StatusNotImplemented)
	}
}

// TestShutdownIsNotHeldOpenByAStream opens a stream, signals the process, and
// checks the server shuts down promptly anyway. The signal is real rather than
// a call into the channel because that is what the channel subscribes to, and
// releasing the streams on it is the behavior under test.
func TestShutdownIsNotHeldOpenByAStream(t *testing.T) {
	h := start(t)
	connID := h.initialize()
	h.stream(connID)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signalling the process: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	if err := h.server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %v; an open stream must not hold it", elapsed)
	}
}
