package acp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/8monkey-ai/momo/internal/core"
)

const token = "operator-token"

type capture struct {
	messages chan core.Message
}

func (c capture) Received(_ context.Context, m core.Message) { c.messages <- m }
func (c capture) Sent(_ context.Context, m core.Message)     { c.messages <- m }

// call is one HTTP request to the endpoint, carrying the operator's token and
// the content negotiation headers unless a test overrides them.
type call struct {
	method  string
	body    string
	auth    string
	headers map[string]string
}

func newCall(method, body string) call {
	return call{method: method, body: body, auth: "Bearer " + token, headers: map[string]string{}}
}

// with adds a header. Each call gets its own header map, so adding to the copy
// this returns cannot affect another call.
func (c call) with(key, value string) call {
	c.headers[key] = value
	return c
}

func (c call) send(t *testing.T, srv *httptest.Server) *http.Response {
	t.Helper()
	r, err := http.NewRequest(c.method, srv.URL, strings.NewReader(c.body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if c.auth != "" {
		r.Header.Set("Authorization", c.auth)
	}
	if c.method == http.MethodPost {
		r.Header.Set("Content-Type", jsonType)
	}
	if c.method == http.MethodGet {
		r.Header.Set("Accept", eventStream)
	}
	for k, v := range c.headers {
		r.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatalf("%s: %v", c.method, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func serving(t *testing.T) (*httptest.Server, *endpoint, capture) {
	t.Helper()
	core := capture{messages: make(chan core.Message, 4)}
	c, err := New(func(v any) error {
		v.(*settings).Token = token
		return nil
	}, core)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e := c.(*endpoint)
	srv := httptest.NewServer(e)
	t.Cleanup(func() {
		e.Close()
		srv.Close()
	})
	return srv, e, core
}

func connections(e *endpoint) int {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	return len(e.hub.conns)
}

func rpcBody(t *testing.T, resp *http.Response) response {
	t.Helper()
	var answer response
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return answer
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize",` +
	`"params":{"protocolVersion":1,"clientCapabilities":{}}}`

func TestNewRequiresToken(t *testing.T) {
	if _, err := New(func(any) error { return nil }, capture{}); err == nil {
		t.Fatal("New succeeded without a token, want an error")
	}
}

func TestDefaultPath(t *testing.T) {
	c, err := New(func(v any) error {
		v.(*settings).Token = token
		return nil
	}, capture{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	routes := c.Routes()
	if len(routes) != 1 || routes[0].Path != "/acp" {
		t.Fatalf("routes = %+v, want one route on /acp", routes)
	}
}

func TestTokenIsRequiredOnEveryMethod(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		for name, auth := range map[string]string{
			"missing": "",
			"wrong":   "Bearer not-the-token",
			"empty":   "Bearer ",
			"basic":   "Basic " + token,
		} {
			t.Run(method+"/"+name, func(t *testing.T) {
				srv, e, _ := serving(t)
				c := newCall(method, initializeBody)
				c.auth = auth
				resp := c.send(t, srv)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
				}
				if got := connections(e); got != 0 {
					t.Errorf("connections = %d, want none created", got)
				}
			})
		}
	}
}

func TestPostRequiresJSONContentType(t *testing.T) {
	srv, _, _ := serving(t)
	resp := newCall(http.MethodPost, initializeBody).with("Content-Type", "text/plain").send(t, srv)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnsupportedMediaType)
	}
}

func TestPostAcceptsContentTypeParameters(t *testing.T) {
	srv, _, _ := serving(t)
	resp := newCall(http.MethodPost, initializeBody).with("Content-Type", "application/json; charset=utf-8").send(t, srv)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestStreamRequiresEventStreamAccept(t *testing.T) {
	srv, _, _ := serving(t)
	resp := newCall(http.MethodGet, "").with("Accept", jsonType).send(t, srv)
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotAcceptable)
	}
}

func TestWebSocketUpgradeIsRefused(t *testing.T) {
	srv, _, _ := serving(t)
	resp := newCall(http.MethodGet, "").with("Upgrade", "websocket").send(t, srv)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotImplemented)
	}
}

func TestBatchIsRefused(t *testing.T) {
	srv, e, _ := serving(t)
	resp := newCall(http.MethodPost, " [ "+initializeBody+" ] ").send(t, srv)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotImplemented)
	}
	if got := connections(e); got != 0 {
		t.Errorf("connections = %d, want none created", got)
	}
}

func TestUnparseableMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{name: "not json", body: "{not json", code: codeParseError},
		{name: "empty body", body: "", code: codeParseError},
		{name: "no jsonrpc version", body: `{"id":1,"method":"initialize"}`, code: codeInvalidRequest},
		{name: "no method", body: `{"jsonrpc":"2.0","id":1}`, code: codeInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := serving(t)
			resp := newCall(http.MethodPost, tc.body).send(t, srv)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
			answer := rpcBody(t, resp)
			if answer.Error == nil || answer.Error.Code != tc.code {
				t.Errorf("error = %+v, want code %d", answer.Error, tc.code)
			}
		})
	}
}

func TestUnknownHTTPMethod(t *testing.T) {
	srv, _, _ := serving(t)
	resp := newCall(http.MethodPut, initializeBody).send(t, srv)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestIdentityHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(connID, sessionID string) call
		want int
	}{
		{
			name: "post without a connection id",
			call: func(string, string) call { return newCall(http.MethodPost, newSessionBody(2)) },
			want: http.StatusBadRequest,
		},
		{
			name: "post with an unknown connection id",
			call: func(string, string) call {
				return newCall(http.MethodPost, newSessionBody(2)).with(connectionHeader, "no-such-connection")
			},
			want: http.StatusNotFound,
		},
		{
			name: "session-scoped post without a session id",
			call: func(connID, sessionID string) call {
				return newCall(http.MethodPost, promptBody(3, sessionID, "hello")).with(connectionHeader, connID)
			},
			want: http.StatusBadRequest,
		},
		{
			name: "session-scoped post with an unknown session id",
			call: func(connID, _ string) call {
				return newCall(http.MethodPost, promptBody(3, "no-such-session", "hello")).
					with(connectionHeader, connID).
					with(sessionHeader, "no-such-session")
			},
			want: http.StatusNotFound,
		},
		{
			name: "stream without a connection id",
			call: func(_, sessionID string) call {
				return newCall(http.MethodGet, "").with(sessionHeader, sessionID)
			},
			want: http.StatusBadRequest,
		},
		{
			name: "stream with a session id from no session",
			call: func(connID, _ string) call {
				return newCall(http.MethodGet, "").
					with(connectionHeader, connID).
					with(sessionHeader, "no-such-session")
			},
			want: http.StatusNotFound,
		},
		{
			name: "delete without a connection id",
			call: func(string, string) call { return newCall(http.MethodDelete, "") },
			want: http.StatusBadRequest,
		},
		{
			name: "delete with an unknown connection id",
			call: func(string, string) call {
				return newCall(http.MethodDelete, "").with(connectionHeader, "no-such-connection")
			},
			want: http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := serving(t)
			connID, sessionID := connect(t, srv)
			resp := tc.call(connID, sessionID).send(t, srv)
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d (%s), want %d", resp.StatusCode, strings.TrimSpace(string(body)), tc.want)
			}
		})
	}
}
