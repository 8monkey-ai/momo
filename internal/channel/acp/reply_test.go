package acp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	wire "github.com/8monkey-ai/momo/internal/acp"
	"github.com/8monkey-ai/momo/internal/core"
)

// notification is the session/update message momo emits, read off the stream as
// the client sees it.
type notification struct {
	Method string            `json:"method"`
	ID     *int              `json:"id"`
	Params wire.UpdateParams `json:"params"`
}

func (s *sse) nextUpdate(t *testing.T) notification {
	t.Helper()
	frame := s.nextFrame(t)
	var n notification
	if err := json.Unmarshal(frame, &n); err != nil {
		t.Fatalf("frame %s: %v", frame, err)
	}
	if n.Method != "session/update" {
		t.Fatalf("frame %s is not a session/update", frame)
	}
	if n.ID != nil {
		t.Fatalf("frame %s carries an id, want a notification", frame)
	}
	return n
}

func (h *harness) prompt(t *testing.T, connID, sessionID, blocks string) {
	t.Helper()
	status(t, h.do(t, request{
		body:   rpc(3, wire.MethodPrompt, `{"sessionId":"`+sessionID+`","prompt":[`+blocks+`]}`),
		connID: connID, sessionID: sessionID,
	}), http.StatusAccepted)
}

func TestEchoRepliesOnTheSessionStreamBeforeAnsweringThePrompt(t *testing.T) {
	h := newEchoHarness(t)
	connID, sessionID, connStream, sessionStream := h.session(t)

	// An audio block is a type momo does not read: it travels back unchanged.
	h.prompt(t, connID, sessionID, `{"type":"text","text":"hello"},`+
		`{"type":"audio","data":"AAAA","mimeType":"audio/wav"},`+
		`{"type":"text","text":"again"}`)

	want := []core.ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "audio", Data: "AAAA", MimeType: "audio/wav"},
		{Type: "text", Text: "again"},
	}
	for i, block := range want {
		got := h.nextReply(t, sessionStream, sessionID)
		if !reflect.DeepEqual(got, block) {
			t.Fatalf("notification %d carried %+v, want %+v", i, got, block)
		}
	}
	// Only after the content does the turn end.
	var completed wire.PromptResult
	unmarshalResult(t, sessionStream.next(t), &completed)
	if completed.StopReason != "end_turn" {
		t.Fatalf("stopReason = %q, want \"end_turn\"", completed.StopReason)
	}
	connStream.silent(t)
}

func (h *harness) nextReply(t *testing.T, s *sse, sessionID string) core.ContentBlock {
	t.Helper()
	n := s.nextUpdate(t)
	if n.Params.SessionID != sessionID {
		t.Fatalf("sessionId = %q, want %q", n.Params.SessionID, sessionID)
	}
	if n.Params.Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("sessionUpdate = %q, want \"agent_message_chunk\"", n.Params.Update.SessionUpdate)
	}
	return n.Params.Update.Content
}

func TestEachSessionIsAnsweredOnItsOwnStream(t *testing.T) {
	h := newEchoHarness(t)
	connID, first, _, firstStream := h.session(t)
	secondStream := h.newSessionOn(t, connID)

	h.prompt(t, connID, first, `{"type":"text","text":"for the first"}`)

	if got := h.nextReply(t, firstStream, first).Text; got != "for the first" {
		t.Fatalf("first session got %q", got)
	}
	secondStream.silent(t)

	h.prompt(t, connID, secondStream.id, `{"type":"text","text":"for the second"}`)
	if got := h.nextReply(t, secondStream.sse, secondStream.id).Text; got != "for the second" {
		t.Fatalf("second session got %q", got)
	}
	// The first session's stream carries its own prompt response and nothing of
	// the second session's turn.
	var completed wire.PromptResult
	unmarshalResult(t, firstStream.next(t), &completed)
	firstStream.silent(t)
}

type session struct {
	id string
	*sse
}

// newSessionOn creates a second session on an existing connection and listens to
// its stream.
func (h *harness) newSessionOn(t *testing.T, connID string) session {
	t.Helper()
	id := h.createSession(t, connID)
	return session{id: id, sse: h.stream(t, connID, id)}
}

// createSession creates a session and returns its id, read off a
// connection-scoped stream opened for that purpose only.
func (h *harness) createSession(t *testing.T, connID string) string {
	t.Helper()
	connStream := h.stream(t, connID, "")
	status(t, h.do(t, request{body: rpc(2, wire.MethodNewSession, "{}"), connID: connID}), http.StatusAccepted)
	var created wire.NewSessionResult
	unmarshalResult(t, connStream.next(t), &created)
	return created.SessionID
}

func TestAPromptWhoseStreamIsGoneStillCompletes(t *testing.T) {
	h := newEchoHarness(t)
	connID := h.initialize(t)
	sessionID := h.createSession(t, connID)

	// Nothing listens to the session's stream, so neither the reply nor the
	// response can be delivered; the POST that carried the prompt is accepted all
	// the same.
	h.prompt(t, connID, sessionID, `{"type":"text","text":"into the void"}`)
}

func TestAFailedTurnAnswersThePromptWithAnError(t *testing.T) {
	h := unserved()
	h.serve(t, core.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), failingAgent{}))
	connID, sessionID, connStream, sessionStream := h.session(t)

	h.prompt(t, connID, sessionID, `{"type":"text","text":"hello"}`)

	resp := sessionStream.next(t)
	if resp.Error == nil {
		t.Fatalf("prompt response = %+v, want an error", resp)
	}
	if !strings.Contains(resp.Error.Message, "the agent exited before it replied") {
		t.Fatalf("error %q does not carry the reason the turn failed", resp.Error.Message)
	}
	connStream.silent(t)
}

func TestReplyReportsAnUndeliveredNotification(t *testing.T) {
	h := unserved()
	e := &endpoint{token: token, conns: h.conns, core: h.core}
	connID := h.conns.newConnection()
	sessionID, _ := h.conns.newSession(connID)

	err := e.reply(connID, sessionID)(context.Background(), core.Text("hi"))
	if err == nil {
		t.Fatal("reply succeeded, want an error: no stream took the notification")
	}
	if !strings.Contains(err.Error(), sessionID) {
		t.Fatalf("error %q does not name the session", err)
	}
}

// blocksAgent replies twice: two blocks in one message, then one block in a
// message of its own.
type blocksAgent struct{}

func (blocksAgent) Turn(_ context.Context, _ core.Message, emit core.Emit) error {
	if err := emit([]core.ContentBlock{{Type: "text", Text: "first"}, {Type: "text", Text: "still first"}}); err != nil {
		return err
	}
	return emit(core.Text("second"))
}

func (blocksAgent) Record(context.Context, core.Message, core.Role) error { return nil }

// TestEachDeliveredMessageCarriesItsOwnMessageID pins what a client joins: the
// blocks of one message share an id, and the next message starts a new one.
func TestEachDeliveredMessageCarriesItsOwnMessageID(t *testing.T) {
	h := unserved()
	h.serve(t, core.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), blocksAgent{}))
	connID, sessionID, _, sessionStream := h.session(t)

	h.prompt(t, connID, sessionID, `{"type":"text","text":"hello"}`)

	ids := make([]string, 0, 3)
	for range 3 {
		id := sessionStream.nextUpdate(t).Params.Update.MessageID
		if id == "" {
			t.Fatal("a delivered message carries no messageId")
		}
		ids = append(ids, id)
	}
	if ids[0] != ids[1] {
		t.Fatalf("the blocks of one message carry %q and %q, want one id", ids[0], ids[1])
	}
	if ids[2] == ids[0] {
		t.Fatalf("the second message carries %q as well, want an id of its own", ids[2])
	}
}
