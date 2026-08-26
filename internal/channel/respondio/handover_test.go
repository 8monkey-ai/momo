package respondio

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

// handover builds a webhook body the way respond.io sends one. Assignee zero
// leaves the conversation unassigned, and an empty source leaves the field out.
func handover(eventType string, assigneeID int64, source string) string {
	contact := `"contact":{"id":12345`
	if assigneeID != 0 {
		contact += `,"assignee":{"id":` + strconv.FormatInt(assigneeID, 10) + `}`
	}
	contact += `}`
	message := `"message":{`
	if source != "" {
		message += `"source":"` + source + `",`
	}
	message += `"message":{"type":"text","text":"hello"}}`
	return `{"event_type":"` + eventType + `",` + contact + `,` + message + `}`
}

type historyRecord struct {
	role    string
	message core.Message
}

type history struct {
	records chan historyRecord
}

func newHistory() history { return history{records: make(chan historyRecord, 2)} }

func (h history) RecordUser(_ context.Context, m core.Message) {
	h.records <- historyRecord{role: "user", message: m}
}

func (h history) RecordAssistant(_ context.Context, m core.Message) {
	h.records <- historyRecord{role: "assistant", message: m}
}

func (h history) next(t *testing.T) historyRecord {
	t.Helper()
	select {
	case got := <-h.records:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was recorded in the session history")
		return historyRecord{}
	}
}

func (h history) silent(t *testing.T) {
	t.Helper()
	select {
	case got := <-h.records:
		t.Fatalf("recorded %+v, want no record", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func noCall(t *testing.T, c capture) {
	t.Helper()
	select {
	case got := <-c.calls:
		t.Fatalf("core was called with %+v, want no call", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// The human who owns the conversation answers alone; the text still reaches the
// agent's session.
func TestAnotherAssigneeGetsNoAgentReply(t *testing.T) {
	c := capture{calls: make(chan call, 1)}
	h := newHistory()
	body := handover(eventReceived, 99, "")
	w := &webhook{secret: secret, core: c, client: &client{}, history: h, momoUserID: 7}
	if got := post(t, w, body, sign(body, secret)).Code; got != 200 {
		t.Fatalf("status = %d, want 200", got)
	}
	got := h.next(t)
	want := historyRecord{role: "user", message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded %+v, want %+v", got, want)
	}
	noCall(t, c)
}

// A message momo answers is not recorded: the turn's own prompt carries it.
func TestMomoOwnsTheConversationItIsAssignedTo(t *testing.T) {
	for name, tc := range map[string]struct {
		assigneeID int64
		momoUserID int64
	}{
		"assigned to momo":            {assigneeID: 7, momoUserID: 7},
		"unassigned":                  {assigneeID: 0, momoUserID: 7},
		"no momo user id configured":  {assigneeID: 99, momoUserID: 0},
		"unassigned and no momo user": {assigneeID: 0, momoUserID: 0},
	} {
		t.Run(name, func(t *testing.T) {
			c := capture{calls: make(chan call, 1)}
			h := newHistory()
			body := handover(eventReceived, tc.assigneeID, "")
			w := &webhook{secret: secret, core: c, client: &client{}, history: h, momoUserID: tc.momoUserID}
			post(t, w, body, sign(body, secret))
			select {
			case got := <-c.calls:
				want := call{direction: "received", message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("core called with %+v, want %+v", got, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("core was never called")
			}
			h.silent(t)
		})
	}
}

// What a respond.io user or a workflow wrote is an assistant message the agent
// never produced.
func TestOutgoingMessagesOfPeopleAndWorkflowsAreRecorded(t *testing.T) {
	for _, source := range []string{"user", "workflow"} {
		t.Run(source, func(t *testing.T) {
			c := capture{calls: make(chan call, 1)}
			h := newHistory()
			body := handover(eventSent, 0, source)
			w := &webhook{secret: secret, core: c, client: &client{}, history: h}
			post(t, w, body, sign(body, secret))
			got := h.next(t)
			want := historyRecord{role: "assistant", message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("recorded %+v, want %+v", got, want)
			}
			noCall(t, c)
		})
	}
}

// momo's own API reply is already in the session, written by the turn that
// produced it.
func TestOtherOutgoingMessagesFollowTheSentFlow(t *testing.T) {
	for _, source := range []string{"bot", "api", "", "invented-later"} {
		t.Run("source "+source, func(t *testing.T) {
			c := capture{calls: make(chan call, 1)}
			h := newHistory()
			body := handover(eventSent, 0, source)
			w := &webhook{secret: secret, core: c, client: &client{}, history: h}
			post(t, w, body, sign(body, secret))
			select {
			case got := <-c.calls:
				if got.direction != "sent" {
					t.Fatalf("core called with %+v, want the sent flow", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("core was never called")
			}
			h.silent(t)
		})
	}
}

func TestARecordedMessageReachesNoContactAndNoComment(t *testing.T) {
	for name, body := range map[string]string{
		"incoming text of a conversation a human owns": handover(eventReceived, 99, ""),
		"outgoing text a human wrote":                  handover(eventSent, 99, "user"),
	} {
		t.Run(name, func(t *testing.T) {
			a := newAPI(t)
			h := newHistory()
			w := &webhook{secret: secret, core: capture{calls: make(chan call, 1)}, client: a.client(), history: h, momoUserID: 7}
			post(t, w, body, sign(body, secret))
			h.next(t)
			a.silent(t)
		})
	}
}

func TestNewRequiresTheSessionHistorySyncForANonzeroMomoUserID(t *testing.T) {
	credentials := func(s *settings) {
		s.ReceivedSecret = "a"
		s.SentSecret = "b"
		s.APIToken = "api-token"
	}
	for name, tc := range map[string]struct {
		momoUserID int64
		history    core.History
		wantError  bool
	}{
		"an assignee id without the extension": {momoUserID: 7, wantError: true},
		"an assignee id with the extension":    {momoUserID: 7, history: newHistory()},
		"no assignee id and no extension":      {},
		"no assignee id with the extension":    {history: newHistory()},
	} {
		t.Run(name, func(t *testing.T) {
			decode := func(v any) error {
				s := v.(*settings)
				credentials(s)
				s.MomoUserID = tc.momoUserID
				return nil
			}
			_, err := New(context.Background(), decode, capture{}, tc.history)
			if tc.wantError && err == nil {
				t.Fatal("New succeeded, want an error about the missing session history sync")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}

func TestNewPassesTheMomoUserIDAndTheHistoryToEachWebhook(t *testing.T) {
	h := newHistory()
	decode := func(v any) error {
		s := v.(*settings)
		s.ReceivedSecret = "a"
		s.SentSecret = "b"
		s.APIToken = "api-token"
		s.MomoUserID = 7
		return nil
	}
	c, err := New(context.Background(), decode, capture{}, h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, route := range c.Routes() {
		w, ok := route.Handler.(*webhook)
		if !ok {
			t.Fatalf("handler of %q is %T, want *webhook", route.Path, route.Handler)
		}
		if w.momoUserID != 7 {
			t.Fatalf("webhook of %q has momo_user_id %d, want 7", route.Path, w.momoUserID)
		}
		if w.history != core.History(h) {
			t.Fatalf("webhook of %q got history %+v, want the configured one", route.Path, w.history)
		}
	}
}
