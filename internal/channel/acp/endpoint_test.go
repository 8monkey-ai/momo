package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	wire "github.com/8monkey-ai/momo/internal/acp"
	"github.com/8monkey-ai/momo/internal/core"
)

// echoAgent answers with the content the message carried, so a test drives the
// reply path with no agent subprocess.
type echoAgent struct{}

func (echoAgent) Turn(_ context.Context, m core.Message) ([]core.ContentBlock, error) {
	return m.Content, nil
}

func echoHandler() core.Handler {
	return core.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), echoAgent{})
}

// failingAgent stands for an agent that exits before it replies.
type failingAgent struct{}

func (failingAgent) Turn(context.Context, core.Message) ([]core.ContentBlock, error) {
	return nil, errors.New("the agent exited before it replied")
}

const token = "operator-token"

type capture struct {
	received chan core.Message
}

func (c capture) Received(_ context.Context, m core.Message, _ core.Reply) error {
	c.received <- m
	return nil
}
func (c capture) Sent(_ context.Context, m core.Message) { c.received <- m }

type harness struct {
	url   string
	conns *connectionManager
	core  capture
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := unserved()
	h.serve(t, h.core)
	return h
}

// newEchoHarness serves the same endpoint with the echo handler, so every prompt
// is answered with the content it carried.
func newEchoHarness(t *testing.T) *harness {
	t.Helper()
	h := unserved()
	h.serve(t, echoHandler())
	return h
}

func unserved() *harness {
	return &harness{
		conns: newConnectionManager(time.Minute, time.Now),
		core:  capture{received: make(chan core.Message, 4)},
	}
}

func (h *harness) serve(t *testing.T, handler core.Handler) {
	t.Helper()
	srv := httptest.NewServer(&endpoint{token: token, conns: h.conns, core: handler})
	t.Cleanup(srv.Close)
	h.url = srv.URL
}

// request describes one HTTP request to the endpoint. The zero value is a
// well-formed POST with the configured token.
type request struct {
	method      string
	body        string
	token       string
	omitToken   bool
	contentType string
	accept      string
	connID      string
	sessionID   string
	upgrade     string
}

func (h *harness) do(t *testing.T, req request) *http.Response {
	t.Helper()
	if req.method == "" {
		req.method = http.MethodPost
	}
	r, err := http.NewRequest(req.method, h.url, strings.NewReader(req.body))
	if err != nil {
		t.Fatal(err)
	}
	if !req.omitToken {
		if req.token == "" {
			req.token = token
		}
		r.Header.Set("Authorization", "Bearer "+req.token)
	}
	if req.method == http.MethodPost && req.contentType == "" {
		req.contentType = "application/json"
	}
	set(r, "Content-Type", req.contentType)
	set(r, "Accept", req.accept)
	set(r, connectionHeader, req.connID)
	set(r, sessionHeader, req.sessionID)
	set(r, "Upgrade", req.upgrade)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s: %v", req.method, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func set(r *http.Request, name, value string) {
	if value != "" {
		r.Header.Set(name, value)
	}
}

func rpc(id int, method, params string) string {
	body := `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"` + method + `"`
	if params != "" {
		body += `,"params":` + params
	}
	return body + "}"
}

func status(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s = %d, want %d (%s)", resp.Request.Method, resp.StatusCode, want, body)
	}
}

// initialize opens a connection and returns its id.
func (h *harness) initialize(t *testing.T) string {
	t.Helper()
	resp := h.do(t, request{body: rpc(1, wire.MethodInitialize, `{"protocolVersion":1,"clientCapabilities":{}}`)})
	status(t, resp, http.StatusOK)
	var body struct {
		Result initializeResult `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("initialize response: %v", err)
	}
	if body.Result.ProtocolVersion != 1 {
		t.Fatalf("protocolVersion = %d, want 1", body.Result.ProtocolVersion)
	}
	if got := resp.Header.Get(connectionHeader); got != body.Result.ConnectionID {
		t.Fatalf("%s header = %q, want the connectionId %q", connectionHeader, got, body.Result.ConnectionID)
	}
	return body.Result.ConnectionID
}

type sse struct {
	frames chan json.RawMessage
}

// stream opens an SSE stream, connection-scoped when sessionID is empty.
func (h *harness) stream(t *testing.T, connID, sessionID string) *sse {
	t.Helper()
	resp := h.do(t, request{
		method: http.MethodGet, accept: "text/event-stream",
		connID: connID, sessionID: sessionID,
	})
	status(t, resp, http.StatusOK)
	s := &sse{frames: make(chan json.RawMessage, 8)}
	go func() {
		defer close(s.frames)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if data, found := strings.CutPrefix(scanner.Text(), "data: "); found {
				s.frames <- json.RawMessage(data)
			}
		}
	}()
	return s
}

func (s *sse) next(t *testing.T) jsonrpc2.Response {
	t.Helper()
	frame := s.nextFrame(t)
	var resp jsonrpc2.Response
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatalf("frame %s: %v", frame, err)
	}
	return resp
}

func (s *sse) nextFrame(t *testing.T) json.RawMessage {
	t.Helper()
	select {
	case frame, open := <-s.frames:
		if !open {
			t.Fatal("the stream closed before a message arrived")
		}
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("no message arrived on the stream")
		return nil
	}
}

func (s *sse) silent(t *testing.T) {
	t.Helper()
	select {
	case frame := <-s.frames:
		t.Fatalf("the stream carried %s, want nothing", frame)
	case <-time.After(100 * time.Millisecond):
	}
}

// session initializes a connection, opens both streams and creates a session.
func (h *harness) session(t *testing.T) (connID, sessionID string, connStream, sessionStream *sse) {
	t.Helper()
	connID = h.initialize(t)
	connStream = h.stream(t, connID, "")
	status(t, h.do(t, request{body: rpc(2, wire.MethodNewSession,
		`{"cwd":"/workspace","mcpServers":[]}`), connID: connID}), http.StatusAccepted)
	// The response to session/new lands on the connection-scoped stream: the
	// client has no session id to open a session-scoped one with yet.
	var created wire.NewSessionResult
	unmarshalResult(t, connStream.next(t), &created)
	if created.SessionID == "" {
		t.Fatal("session/new returned no sessionId")
	}
	return connID, created.SessionID, connStream, h.stream(t, connID, created.SessionID)
}

func unmarshalResult(t *testing.T, resp jsonrpc2.Response, v any) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("response carried error %v, want a result", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("response carried no result")
	}
	if err := json.Unmarshal(*resp.Result, v); err != nil {
		t.Fatal(err)
	}
}

func TestTokenIsRequiredOnEveryMethod(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		for _, tc := range []struct {
			name string
			req  request
		}{
			{name: "missing", req: request{omitToken: true}},
			{name: "wrong", req: request{token: "guessed"}},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t)
				req := tc.req
				req.method = method
				req.body = rpc(1, wire.MethodInitialize, "{}")
				req.accept = "text/event-stream"
				status(t, h.do(t, req), http.StatusUnauthorized)
				if len(h.conns.conns) != 0 {
					t.Fatalf("connections = %d, want none created", len(h.conns.conns))
				}
			})
		}
	}
}

func TestTokenSchemeIsCaseInsensitive(t *testing.T) {
	// RFC 7235 makes the scheme token case-insensitive, and clients do send it
	// lowercased.
	h := newHarness(t)
	r, err := http.NewRequest(http.MethodPost, h.url,
		strings.NewReader(rpc(1, wire.MethodInitialize, "{}")))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "bearer "+token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	status(t, resp, http.StatusOK)
}

func TestContentNegotiationRejections(t *testing.T) {
	h := newHarness(t)
	t.Run("POST without json content type", func(t *testing.T) {
		status(t, h.do(t, request{contentType: "text/plain", body: rpc(1, wire.MethodInitialize, "{}")}),
			http.StatusUnsupportedMediaType)
	})
	t.Run("GET without event-stream accept", func(t *testing.T) {
		status(t, h.do(t, request{method: http.MethodGet, accept: "application/json", connID: h.initialize(t)}),
			http.StatusNotAcceptable)
	})
	t.Run("GET asking for a websocket upgrade", func(t *testing.T) {
		status(t, h.do(t, request{
			method: http.MethodGet, accept: "text/event-stream",
			upgrade: "websocket", connID: h.initialize(t),
		}), http.StatusNotImplemented)
	})
}

func TestIdentityHeaderRejections(t *testing.T) {
	h := newHarness(t)
	connID, sessionID, _, _ := h.session(t)
	other := h.initialize(t)

	for _, tc := range []struct {
		name string
		req  request
		want int
	}{
		{
			name: "POST without a connection id",
			req:  request{body: rpc(3, wire.MethodNewSession, "{}")},
			want: http.StatusBadRequest,
		},
		{
			name: "POST with an unknown connection id",
			req:  request{body: rpc(3, wire.MethodNewSession, "{}"), connID: "0123456789abcdef"},
			want: http.StatusNotFound,
		},
		{
			name: "session-scoped POST without a session id",
			req:  request{body: rpc(3, wire.MethodPrompt, `{"prompt":[{"type":"text","text":"hi"}]}`), connID: connID},
			want: http.StatusBadRequest,
		},
		{
			name: "session-scoped POST with a session of another connection",
			req: request{body: rpc(3, wire.MethodPrompt, `{"prompt":[{"type":"text","text":"hi"}]}`),
				connID: other, sessionID: sessionID},
			want: http.StatusNotFound,
		},
		{
			name: "GET without a connection id",
			req:  request{method: http.MethodGet, accept: "text/event-stream"},
			want: http.StatusBadRequest,
		},
		{
			name: "GET with a session of another connection",
			req: request{method: http.MethodGet, accept: "text/event-stream",
				connID: other, sessionID: sessionID},
			want: http.StatusNotFound,
		},
		{
			name: "DELETE without a connection id",
			req:  request{method: http.MethodDelete},
			want: http.StatusBadRequest,
		},
		{
			name: "DELETE with an unknown connection id",
			req:  request{method: http.MethodDelete, connID: "0123456789abcdef"},
			want: http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status(t, h.do(t, tc.req), tc.want)
		})
	}
}

func TestBatchIsRefused(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, request{body: `[` + rpc(1, wire.MethodInitialize, "{}") + `]`})
	status(t, resp, http.StatusNotImplemented)
	if code := rpcErrorCode(t, resp); code != jsonrpc2.CodeInvalidRequest {
		t.Fatalf("code = %d, want %d", code, jsonrpc2.CodeInvalidRequest)
	}
}

func TestMalformedBodyIsAnswered(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		body string
		code int64
	}{
		{body: "", code: jsonrpc2.CodeParseError},
		{body: "not json", code: jsonrpc2.CodeParseError},
		{body: `{"jsonrpc":"2.0"`, code: jsonrpc2.CodeParseError},
		// Valid JSON, but no method to dispatch on.
		{body: `{"jsonrpc":"2.0","id":1}`, code: jsonrpc2.CodeInvalidRequest},
	} {
		resp := h.do(t, request{body: tc.body})
		status(t, resp, http.StatusBadRequest)
		if code := rpcErrorCode(t, resp); code != tc.code {
			t.Fatalf("body %q: code = %d, want %d", tc.body, code, tc.code)
		}
	}
}

func TestMethodsThatNeedAnIdRefuseANotification(t *testing.T) {
	h := newHarness(t)
	connID, sessionID, _, _ := h.session(t)
	for _, tc := range []struct {
		method string
		req    request
	}{
		{method: wire.MethodInitialize},
		{method: wire.MethodNewSession, req: request{connID: connID}},
		{method: wire.MethodPrompt, req: request{connID: connID, sessionID: sessionID}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			req := tc.req
			req.body = `{"jsonrpc":"2.0","method":"` + tc.method + `","params":{}}`
			resp := h.do(t, req)
			status(t, resp, http.StatusBadRequest)
			if code := rpcErrorCode(t, resp); code != jsonrpc2.CodeInvalidRequest {
				t.Fatalf("code = %d, want %d", code, jsonrpc2.CodeInvalidRequest)
			}
		})
	}
	t.Run("session/cancel is accepted without an id", func(t *testing.T) {
		cancelConn, cancelSession, _, sessionStream := h.session(t)
		status(t, h.do(t, request{
			body:   `{"jsonrpc":"2.0","method":"` + wire.MethodCancel + `","params":{"sessionId":"` + cancelSession + `"}}`,
			connID: cancelConn, sessionID: cancelSession,
		}), http.StatusAccepted)
		sessionStream.silent(t)
	})
}

func rpcErrorCode(t *testing.T, resp *http.Response) int64 {
	t.Helper()
	var body struct {
		ID    *int            `json:"id"`
		Error *jsonrpc2.Error `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("response body: %v", err)
	}
	if body.Error == nil {
		t.Fatal("response carried no JSON-RPC error")
	}
	if body.ID != nil {
		t.Fatalf("id = %v, want null: momo did not know the id it was refusing", *body.ID)
	}
	return body.Error.Code
}

func TestPromptReachesTheCoreWithItsBlocksAsSent(t *testing.T) {
	h := newHarness(t)
	connID, sessionID, connStream, sessionStream := h.session(t)

	// An audio block and a resource link are block types momo does not read; they
	// travel to the core unchanged.
	prompt := `{"sessionId":"` + sessionID + `","prompt":[` +
		`{"type":"text","text":"hello"},` +
		`{"type":"audio","data":"AAAA","mimeType":"audio/wav"},` +
		`{"type":"resource","resource":{"uri":"file:///notes.md","text":"notes"}},` +
		`{"type":"resource_link","uri":"file:///notes.md","name":"notes.md"}]}`
	status(t, h.do(t, request{body: rpc(3, wire.MethodPrompt, prompt), connID: connID, sessionID: sessionID}),
		http.StatusAccepted)

	want := core.Message{Conversation: sessionID, Content: []core.ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "audio", Data: "AAAA", MimeType: "audio/wav"},
		{Type: "resource", Resource: &core.Resource{URI: "file:///notes.md", Text: "notes"}},
		{Type: "resource_link", URI: "file:///notes.md", Name: "notes.md"},
	}}
	select {
	case got := <-h.core.received:
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("core received %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the core was never called")
	}

	// The prompt response lands on the session-scoped stream, not the
	// connection-scoped one.
	var completed wire.PromptResult
	unmarshalResult(t, sessionStream.next(t), &completed)
	if completed.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want \"end_turn\"", completed.StopReason)
	}
	connStream.silent(t)
}

func TestPromptWithoutUsableContentIsInvalidParams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params string
	}{
		{name: "no blocks", params: `{"prompt":[]}`},
		{name: "block without a type", params: `{"prompt":[{"text":"hello"}]}`},
		{name: "no params", params: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			connID, sessionID, _, sessionStream := h.session(t)
			status(t, h.do(t, request{body: rpc(3, wire.MethodPrompt, tc.params),
				connID: connID, sessionID: sessionID}), http.StatusAccepted)

			if code := sessionStream.next(t).Error.Code; code != jsonrpc2.CodeInvalidParams {
				t.Fatalf("code = %d, want %d", code, jsonrpc2.CodeInvalidParams)
			}
			select {
			case got := <-h.core.received:
				t.Fatalf("the core received %+v, want no call", got)
			default:
			}
		})
	}
}

func TestUnimplementedMethodLeavesTheConnectionUsable(t *testing.T) {
	h := newHarness(t)
	connID, sessionID, connStream, sessionStream := h.session(t)

	status(t, h.do(t, request{body: rpc(3, "session/load", `{"sessionId":"x"}`), connID: connID}),
		http.StatusAccepted)
	if code := connStream.next(t).Error.Code; code != jsonrpc2.CodeMethodNotFound {
		t.Fatalf("code = %d, want %d", code, jsonrpc2.CodeMethodNotFound)
	}

	status(t, h.do(t, request{body: rpc(4, wire.MethodPrompt, `{"prompt":[{"type":"text","text":"still here"}]}`),
		connID: connID, sessionID: sessionID}), http.StatusAccepted)
	var completed wire.PromptResult
	unmarshalResult(t, sessionStream.next(t), &completed)
}

func TestDeleteReleasesSessionsAndClosesStreams(t *testing.T) {
	h := newHarness(t)
	connID, sessionID, connStream, sessionStream := h.session(t)

	status(t, h.do(t, request{method: http.MethodDelete, connID: connID}), http.StatusNoContent)

	for name, s := range map[string]*sse{"connection-scoped": connStream, "session-scoped": sessionStream} {
		select {
		case _, open := <-s.frames:
			if open {
				t.Fatalf("the %s stream carried a message, want it closed", name)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("the %s stream stayed open after DELETE", name)
		}
	}
	status(t, h.do(t, request{body: rpc(3, wire.MethodPrompt, `{"prompt":[{"type":"text","text":"hi"}]}`),
		connID: connID, sessionID: sessionID}), http.StatusNotFound)
}

func TestNewRejectsUnusableSettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*settings)
	}{
		{name: "no token", apply: func(*settings) {}},
		// A non-positive grace would panic the sweep's ticker.
		{name: "zero grace", apply: func(s *settings) { s.Token = token; s.ConnectionGrace = 0 }},
		{name: "negative grace", apply: func(s *settings) { s.Token = token; s.ConnectionGrace = -time.Minute }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decode := func(v any) error {
				tc.apply(v.(*settings))
				return nil
			}
			if _, err := New(context.Background(), decode, capture{}); err == nil {
				t.Fatal("New succeeded, want an error naming the unusable setting")
			}
		})
	}
}

func TestNewServesTheConfiguredPath(t *testing.T) {
	decode := func(v any) error {
		s, ok := v.(*settings)
		if !ok {
			t.Fatalf("decoded into %T, want *settings", v)
		}
		s.Token = token
		return nil
	}
	c, err := New(context.Background(), decode, capture{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	routes := c.Routes()
	if len(routes) != 1 || routes[0].Path != "/v1/acp" {
		t.Fatalf("routes = %+v, want the single default path /v1/acp", routes)
	}
}
