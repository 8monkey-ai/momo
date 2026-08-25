package respondio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
	"github.com/8monkey-ai/momo/internal/extension/sessionhistory"
)

// answering is a handler that answers an incoming message with one reply, so a
// test sees both the call and the message the contact gets.
type answering struct {
	calls chan call
}

func (a answering) Received(ctx context.Context, m core.Message, reply core.Reply) error {
	a.calls <- call{direction: "received", message: m}
	return reply(ctx, core.Text("answer"))
}

func (a answering) Sent(_ context.Context, m core.Message) {
	a.calls <- call{direction: "sent", message: m}
}

type record struct {
	conversation string
	role         sessionhistory.Role
	text         string
}

// recording is a session-history Recorder that keeps every record it got and
// fails with err.
type recording struct {
	records chan record
	err     error
}

func (r recording) Record(_ context.Context, m core.Message, role sessionhistory.Role) error {
	r.records <- record{conversation: m.Conversation, role: role, text: core.TextOf(m.Content)}
	return r.err
}

// handover is a webhook of a channel with the session-history extension enabled,
// the calls its handler got, the records its recorder got, and the bodies it sent
// to the respond.io API.
type handover struct {
	hook    *webhook
	calls   chan call
	records chan record
	api     chan string
}

func newHandover(t *testing.T, assigneeID int64, failing error) *handover {
	t.Helper()
	h := &handover{
		calls:   make(chan call, 2),
		records: make(chan record, 2),
		api:     make(chan string, 2),
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the API call: %v", err)
			return
		}
		h.api <- r.URL.Path + " " + string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(api.Close)
	h.hook = &webhook{
		secret:     secret,
		core:       answering{calls: h.calls},
		client:     &client{url: api.URL, token: "token", http: api.Client()},
		assigneeID: assigneeID,
		history:    recording{records: h.records, err: failing},
	}
	return h
}

// payloadWith builds one webhook payload. An empty assignee or sender leaves that
// member out, the way respond.io omits it.
func payloadWith(eventType, assignee, sender string) string {
	contact := `"contact":{"id":12345}`
	if assignee != "" {
		contact = fmt.Sprintf(`"contact":{"id":12345,"assignee":%s}`, assignee)
	}
	body := fmt.Sprintf(`{"event_type":%q,%s,"message":{"message":{"type":"text","text":"hello"}}`, eventType, contact)
	if sender != "" {
		body += fmt.Sprintf(`,"sender":{"source":%q}`, sender)
	}
	return body + "}"
}

func deliver(t *testing.T, hook *webhook, body string) {
	t.Helper()
	if got := post(t, hook, body, sign(body, secret)).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
}

// wantRecord waits for one record and asserts nothing was handled instead.
func wantRecord(t *testing.T, h *handover, role sessionhistory.Role) {
	t.Helper()
	select {
	case got := <-h.records:
		want := record{conversation: "12345", role: role, text: "hello"}
		if got != want {
			t.Fatalf("recorded %+v, want %+v", got, want)
		}
	case got := <-h.calls:
		t.Fatalf("the handler was called with %+v, want a record", got)
	case <-time.After(time.Second):
		t.Fatal("nothing was recorded")
	}
}

// wantCall waits for one handler call and asserts nothing was recorded instead.
func wantCall(t *testing.T, h *handover, direction string) {
	t.Helper()
	select {
	case got := <-h.calls:
		want := call{direction: direction, message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
		if got.direction != want.direction || got.message.Conversation != want.message.Conversation {
			t.Fatalf("the handler got %+v, want %+v", got, want)
		}
	case got := <-h.records:
		t.Fatalf("the message was recorded as %+v, want a %s call", got, direction)
	case <-time.After(time.Second):
		t.Fatalf("the handler was never called for the %s message", direction)
	}
}

func wantAnswer(t *testing.T, h *handover) {
	t.Helper()
	select {
	case got := <-h.api:
		want := `/contact/id:12345/message {"message":{"text":"answer","type":"text"}}`
		if got != want {
			t.Fatalf("respond.io received %s, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("no answer reached the contact")
	}
}

func wantNothingSent(t *testing.T, h *handover) {
	t.Helper()
	select {
	case got := <-h.api:
		t.Fatalf("respond.io received %s, want no call", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAnAssignedContactIsRecordedAndNotAnswered(t *testing.T) {
	h := newHandover(t, 7, nil)

	deliver(t, h.hook, payloadWith(eventReceived, `{"id":99}`, ""))

	wantRecord(t, h, sessionhistory.RoleUser)
	wantNothingSent(t, h)
}

func TestMomoAnswersTheContactsItOwns(t *testing.T) {
	for name, assignee := range map[string]string{
		"assigned to momo": `{"id":7}`,
		"omitted assignee": "",
		"null assignee":    "null",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHandover(t, 7, nil)

			deliver(t, h.hook, payloadWith(eventReceived, assignee, ""))

			wantCall(t, h, "received")
			wantAnswer(t, h)
		})
	}
}

func TestTheSenderDecidesWhetherAnOutgoingMessageIsRecorded(t *testing.T) {
	for name, tc := range map[string]struct {
		sender   string
		assignee string
		recorded bool
	}{
		"an operator":         {sender: "user", assignee: `{"id":99}`, recorded: true},
		"an operator of momo": {sender: "user", assignee: `{"id":7}`, recorded: true},
		"a workflow":          {sender: "workflow", assignee: `{"id":7}`, recorded: true},
		"the api":             {sender: "api", assignee: `{"id":99}`},
		"an unknown sender":   {sender: "bot", assignee: `{"id":7}`},
		"no sender":           {assignee: `{"id":7}`},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHandover(t, 7, nil)

			deliver(t, h.hook, payloadWith(eventSent, tc.assignee, tc.sender))

			if tc.recorded {
				wantRecord(t, h, sessionhistory.RoleAssistant)
				return
			}
			wantCall(t, h, "sent")
		})
	}
}

func TestWithoutAnAssigneeIDEveryContactBelongsToMomo(t *testing.T) {
	for name, assignee := range map[string]string{
		"another assignee": `{"id":99}`,
		"no assignee":      "",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHandover(t, 0, nil)

			deliver(t, h.hook, payloadWith(eventReceived, assignee, ""))

			wantCall(t, h, "received")
			wantAnswer(t, h)
		})
	}
}

func TestWithoutAnAssigneeIDOutgoingMessagesAreStillRecordedBySender(t *testing.T) {
	for name, tc := range map[string]struct {
		sender   string
		recorded bool
	}{
		"an operator": {sender: "user", recorded: true},
		"a workflow":  {sender: "workflow", recorded: true},
		"the api":     {sender: "api"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHandover(t, 0, nil)

			deliver(t, h.hook, payloadWith(eventSent, "", tc.sender))

			if tc.recorded {
				wantRecord(t, h, sessionhistory.RoleAssistant)
				return
			}
			wantCall(t, h, "sent")
		})
	}
}

func TestAFailedRecordIsNotReportedToRespondio(t *testing.T) {
	h := newHandover(t, 7, errors.New("the agent refused the command"))

	deliver(t, h.hook, payloadWith(eventReceived, `{"id":99}`, ""))

	wantRecord(t, h, sessionhistory.RoleUser)
	wantNothingSent(t, h)
}

// TestADisabledExtensionKeepsEveryMessageWithTheHandler pins the deployment
// without the extension: nothing is recorded, and an assigned contact is answered
// like any other.
func TestADisabledExtensionKeepsEveryMessageWithTheHandler(t *testing.T) {
	for name, body := range map[string]string{
		"an assigned incoming message": payloadWith(eventReceived, `{"id":99}`, ""),
		"an operator's message":        payloadWith(eventSent, `{"id":99}`, "user"),
		"a workflow's message":         payloadWith(eventSent, "", "workflow"),
		"an api message":               payloadWith(eventSent, "", "api"),
	} {
		t.Run(name, func(t *testing.T) {
			calls := make(chan call, 2)
			hook := &webhook{secret: secret, core: capture{calls: calls}, client: &client{}}

			deliver(t, hook, body)

			select {
			case got := <-calls:
				if got.direction == "" {
					t.Fatalf("the handler got %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("the handler was never called")
			}
		})
	}
}

func TestNewRefusesAnAssigneeIDWithoutTheExtension(t *testing.T) {
	decode := func(v any) error {
		s := v.(*settings)
		s.ReceivedSecret = "a"
		s.SentSecret = "b"
		s.APIToken = "token"
		s.AssigneeID = 7
		return nil
	}
	if _, err := New(context.Background(), decode, capture{}, nil); err == nil {
		t.Fatal("New succeeded, want an error naming the session-history extension")
	}
}
