package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newSessionBody(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new",`+
		`"params":{"cwd":"/workspace","mcpServers":[]}}`, id)
}

func promptBody(id int, sessionID, text string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt",`+
		`"params":{"sessionId":%q,"prompt":[{"type":"text","text":%q}]}}`, id, sessionID, text)
}

// events is the SSE stream a test opened, as the messages momo wrote to it.
type events struct {
	ch chan string
}

// streamed is a message a test read off a stream. The result stays raw so the
// test can decode it into the shape the method it answers is specified to return.
type streamed struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func openStream(t *testing.T, srv *httptest.Server, connID, sessionID string) *events {
	t.Helper()
	c := newCall(http.MethodGet, "").with(connectionHeader, connID)
	if sessionID != "" {
		c = c.with(sessionHeader, sessionID)
	}
	resp := c.send(t, srv)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); got != eventStream {
		t.Fatalf("stream content type = %q, want %q", got, eventStream)
	}
	e := &events{ch: make(chan string, streamBuffer)}
	go func() {
		defer close(e.ch)
		lines := bufio.NewScanner(resp.Body)
		for lines.Scan() {
			if data, isData := strings.CutPrefix(lines.Text(), "data: "); isData {
				e.ch <- data
			}
		}
	}()
	return e
}

func (e *events) next(t *testing.T) streamed {
	t.Helper()
	select {
	case data, open := <-e.ch:
		if !open {
			t.Fatal("stream ended before a message arrived")
		}
		var answer streamed
		if err := json.Unmarshal([]byte(data), &answer); err != nil {
			t.Fatalf("stream carried %q: %v", data, err)
		}
		return answer
	case <-time.After(2 * time.Second):
		t.Fatal("no message on the stream")
		return streamed{}
	}
}

// quiet asserts nothing is waiting on the stream. momo writes to the stream
// before it answers the POST, so a message that was going to arrive is already
// there by the time a test can call this.
func (e *events) quiet(t *testing.T) {
	t.Helper()
	select {
	case data := <-e.ch:
		t.Fatalf("stream carried %q, want nothing", data)
	case <-time.After(50 * time.Millisecond):
	}
}

func (e *events) ended(t *testing.T) {
	t.Helper()
	select {
	case _, open := <-e.ch:
		if open {
			t.Fatal("stream carried a message, want it closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream stayed open")
	}
}

func initialize(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp := newCall(http.MethodPost, initializeBody).send(t, srv)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	connID := resp.Header.Get(connectionHeader)
	if connID == "" {
		t.Fatalf("initialize answered no %s header", connectionHeader)
	}
	var body struct {
		Result initializeResult `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if body.Result.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %d, want %d", body.Result.ProtocolVersion, protocolVersion)
	}
	if body.Result.ConnectionID != connID {
		t.Errorf("connectionId = %q, want the header's %q", body.Result.ConnectionID, connID)
	}
	return connID
}

// connect walks the whole client flow: initialize, the connection-scoped stream,
// then a session, whose id arrives on that stream.
func connect(t *testing.T, srv *httptest.Server) (string, string) {
	t.Helper()
	connID := initialize(t, srv)
	stream := openStream(t, srv, connID, "")
	sessionID := newSession(t, srv, connID, stream)
	return connID, sessionID
}

func newSession(t *testing.T, srv *httptest.Server, connID string, connStream *events) string {
	t.Helper()
	resp := newCall(http.MethodPost, newSessionBody(2)).with(connectionHeader, connID).send(t, srv)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("session/new status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	answer := connStream.next(t)
	var created newSessionResult
	if err := json.Unmarshal(answer.Result, &created); err != nil {
		t.Fatalf("decode session/new result: %v", err)
	}
	if created.SessionID == "" {
		t.Fatal("session/new answered no sessionId")
	}
	return created.SessionID
}

func TestPromptReachesTheCoreAndAnswersOnTheSessionStream(t *testing.T) {
	srv, _, core := serving(t)
	connID := initialize(t, srv)
	connStream := openStream(t, srv, connID, "")
	sessionID := newSession(t, srv, connID, connStream)
	sessionStream := openStream(t, srv, connID, sessionID)

	resp := newCall(http.MethodPost, promptBody(3, sessionID, "hello momo")).
		with(connectionHeader, connID).
		with(sessionHeader, sessionID).
		send(t, srv)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("session/prompt status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	select {
	case m := <-core.messages:
		if m.Contact != sessionID {
			t.Errorf("contact = %q, want the session id %q", m.Contact, sessionID)
		}
		if m.Text != "hello momo" {
			t.Errorf("text = %q, want %q", m.Text, "hello momo")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the prompt did not reach the core")
	}

	answer := sessionStream.next(t)
	var ended promptResult
	if err := json.Unmarshal(answer.Result, &ended); err != nil {
		t.Fatalf("decode session/prompt result: %v", err)
	}
	if ended.StopReason != "end_turn" {
		t.Errorf("stopReason = %q, want %q", ended.StopReason, "end_turn")
	}
	// The session's response belongs to the session's stream alone.
	connStream.quiet(t)
}

func TestPromptWithoutTextLeavesTheCoreNothing(t *testing.T) {
	srv, _, core := serving(t)
	connID, sessionID := connect(t, srv)
	sessionStream := openStream(t, srv, connID, sessionID)

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":`+
		`{"sessionId":%q,"prompt":[{"type":"resource_link","uri":"file:///notes.md","name":"notes"}]}}`, sessionID)
	newCall(http.MethodPost, body).with(connectionHeader, connID).with(sessionHeader, sessionID).send(t, srv)

	if answer := sessionStream.next(t); answer.Error != nil {
		t.Fatalf("session/prompt failed: %+v", answer.Error)
	}
	select {
	case m := <-core.messages:
		t.Errorf("core received %+v, want nothing", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPromptWithAMismatchedSessionID(t *testing.T) {
	srv, _, _ := serving(t)
	connID, sessionID := connect(t, srv)
	sessionStream := openStream(t, srv, connID, sessionID)

	newCall(http.MethodPost, promptBody(3, "some-other-session", "hello")).
		with(connectionHeader, connID).
		with(sessionHeader, sessionID).
		send(t, srv)

	answer := sessionStream.next(t)
	if answer.Error == nil || answer.Error.Code != codeInvalidParams {
		t.Errorf("error = %+v, want code %d", answer.Error, codeInvalidParams)
	}
}

func TestCancelIsANotificationAndAnswersNothing(t *testing.T) {
	srv, _, _ := serving(t)
	connID, sessionID := connect(t, srv)
	sessionStream := openStream(t, srv, connID, sessionID)

	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":%q}}`, sessionID)
	resp := newCall(http.MethodPost, body).with(connectionHeader, connID).with(sessionHeader, sessionID).send(t, srv)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("session/cancel status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	sessionStream.quiet(t)
}

func TestUnimplementedMethodLeavesTheConnectionUsable(t *testing.T) {
	srv, _, _ := serving(t)
	connID := initialize(t, srv)
	connStream := openStream(t, srv, connID, "")

	resp := newCall(http.MethodPost, `{"jsonrpc":"2.0","id":7,"method":"session/load","params":{}}`).
		with(connectionHeader, connID).send(t, srv)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	answer := connStream.next(t)
	if answer.Error == nil || answer.Error.Code != codeMethodNotFound {
		t.Fatalf("error = %+v, want code %d", answer.Error, codeMethodNotFound)
	}
	if string(answer.ID) != "7" {
		t.Errorf("id = %s, want 7", answer.ID)
	}
	// The connection still works after a method it does not know.
	newSession(t, srv, connID, connStream)
}

func TestDeleteReleasesTheConnection(t *testing.T) {
	srv, e, _ := serving(t)
	connID, sessionID := connect(t, srv)
	connStream := openStream(t, srv, connID, "")
	sessionStream := openStream(t, srv, connID, sessionID)

	resp := newCall(http.MethodDelete, "").with(connectionHeader, connID).send(t, srv)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	connStream.ended(t)
	sessionStream.ended(t)
	if got := connections(e); got != 0 {
		t.Errorf("connections = %d, want none", got)
	}
	after := newCall(http.MethodPost, newSessionBody(9)).with(connectionHeader, connID).send(t, srv)
	if after.StatusCode != http.StatusNotFound {
		t.Errorf("status after delete = %d, want %d", after.StatusCode, http.StatusNotFound)
	}
}

func TestReopeningAStreamReplacesTheOldOne(t *testing.T) {
	srv, _, _ := serving(t)
	connID := initialize(t, srv)
	first := openStream(t, srv, connID, "")
	second := openStream(t, srv, connID, "")
	first.ended(t)
	newSession(t, srv, connID, second)
}

func TestPromptWithUnusableParams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params string
	}{
		{name: "no params", params: ""},
		{name: "params are not an object", params: `,"params":[]`},
		{name: "prompt is not a list", params: `,"params":{"prompt":"hello"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, core := serving(t)
			connID, sessionID := connect(t, srv)
			sessionStream := openStream(t, srv, connID, sessionID)

			body := `{"jsonrpc":"2.0","id":3,"method":"session/prompt"` + tc.params + `}`
			newCall(http.MethodPost, body).with(connectionHeader, connID).with(sessionHeader, sessionID).send(t, srv)

			answer := sessionStream.next(t)
			if answer.Error == nil || answer.Error.Code != codeInvalidParams {
				t.Errorf("error = %+v, want code %d", answer.Error, codeInvalidParams)
			}
			select {
			case m := <-core.messages:
				t.Errorf("core received %+v, want nothing", m)
			default:
			}
		})
	}
}

func TestSessionsAreBounded(t *testing.T) {
	srv, _, _ := serving(t)
	connID := initialize(t, srv)
	connStream := openStream(t, srv, connID, "")
	for range maxSessions {
		newSession(t, srv, connID, connStream)
	}
	newCall(http.MethodPost, newSessionBody(99)).with(connectionHeader, connID).send(t, srv)
	answer := connStream.next(t)
	if answer.Error == nil || answer.Error.Code != codeInternalError {
		t.Errorf("error = %+v, want code %d", answer.Error, codeInternalError)
	}
}

func TestConnectionsAreBounded(t *testing.T) {
	srv, e, _ := serving(t)
	for range maxConnections {
		initialize(t, srv)
	}
	resp := newCall(http.MethodPost, initializeBody).send(t, srv)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if got := connections(e); got != maxConnections {
		t.Errorf("connections = %d, want %d", got, maxConnections)
	}
}

func TestShutdownDoesNotWaitForAnOpenStream(t *testing.T) {
	srv, e, _ := serving(t)
	connID := initialize(t, srv)
	stream := openStream(t, srv, connID, "")

	// What cmd/momo does on SIGTERM: release the channel's long-lived responses,
	// then drain. Without the release, Shutdown would wait for the stream, which
	// never ends on its own.
	e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Config.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	stream.ended(t)
}
