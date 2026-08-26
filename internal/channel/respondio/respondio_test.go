package respondio

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

const secret = "signing-key"

type call struct {
	direction string
	message   core.Message
	ctxErr    error
}

type capture struct {
	calls chan call
}

func (c capture) Received(ctx context.Context, m core.Message, _ core.Reply) error {
	c.calls <- call{direction: "received", message: m, ctxErr: ctx.Err()}
	return nil
}

func (c capture) Sent(ctx context.Context, m core.Message) {
	c.calls <- call{direction: "sent", message: m, ctxErr: ctx.Err()}
}

// records stands in for the session history extension, reporting on the same
// channel as the core calls: one payload leads to one of the four, so the channel
// says what the webhook decided.
type records struct {
	calls chan call
}

func (r records) RecordUser(_ context.Context, m core.Message) {
	r.calls <- call{direction: "record user", message: m}
}

func (r records) RecordAssistant(_ context.Context, m core.Message) {
	r.calls <- call{direction: "record assistant", message: m}
}

func sign(body, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func post(t *testing.T, h http.Handler, body, signature string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/respondio/received", strings.NewReader(body))
	if signature != "" {
		r.Header.Set(signatureHeader, signature)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func payload(eventType string) string {
	return `{"event_type":"` + eventType + `","contact":{"id":12345},` +
		`"message":{"message":{"type":"text","text":"hello"}}}`
}

func TestSignature(t *testing.T) {
	body := payload(eventReceived)
	for _, tc := range []struct {
		name      string
		signature string
		status    int
	}{
		{name: "valid", signature: sign(body, secret), status: http.StatusOK},
		{name: "invalid", signature: sign(body, "other-key"), status: http.StatusUnauthorized},
		{name: "missing", signature: "", status: http.StatusUnauthorized},
		{name: "not base64", signature: "!!!", status: http.StatusUnauthorized},
		{name: "signs a different body", signature: sign("tampered", secret), status: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := capture{calls: make(chan call, 1)}
			if got := post(t, &webhook{secret: secret, core: c, client: &client{}}, body, tc.signature).Code; got != tc.status {
				t.Fatalf("status = %d, want %d", got, tc.status)
			}
		})
	}
}

func TestDispatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		direction string
	}{
		{name: "incoming message", body: payload(eventReceived), direction: "received"},
		{name: "outgoing message", body: payload(eventSent), direction: "sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := capture{calls: make(chan call, 1)}
			if got := post(t, &webhook{secret: secret, core: c, client: &client{}}, tc.body, sign(tc.body, secret)).Code; got != http.StatusOK {
				t.Fatalf("status = %d, want %d", got, http.StatusOK)
			}
			select {
			case got := <-c.calls:
				want := call{direction: tc.direction, message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("core called with %+v, want %+v", got, want)
				}
			case <-time.After(time.Second):
				t.Fatal("core was never called")
			}
		})
	}
}

func TestDispatchSurvivesTheRequestEnding(t *testing.T) {
	body := payload(eventReceived)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/respondio/received", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set(signatureHeader, sign(body, secret))
	// The request is over before the handler runs, as it is once respond.io has
	// its response.
	cancel()
	c := capture{calls: make(chan call, 1)}
	(&webhook{secret: secret, core: c, client: &client{}}).ServeHTTP(httptest.NewRecorder(), r)

	select {
	case got := <-c.calls:
		if got.ctxErr != nil {
			t.Fatalf("core got a cancelled context (%v); the message must be acted on after the response", got.ctxErr)
		}
	case <-time.After(time.Second):
		t.Fatal("core was never called")
	}
}

func TestIgnoredEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "unknown event type", body: payload("contact.updated")},
		{name: "future event type", body: payload("something.invented.later")},
		{name: "message without text", body: `{"event_type":"message.received","contact":{"id":1},` +
			`"message":{"message":{"type":"image"}}}`},
		{name: "empty object", body: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := capture{calls: make(chan call, 1)}
			// A 2xx keeps respond.io from retrying an event momo will never act on.
			if got := post(t, &webhook{secret: secret, core: c, client: &client{}}, tc.body, sign(tc.body, secret)).Code; got != http.StatusOK {
				t.Fatalf("status = %d, want %d", got, http.StatusOK)
			}
			select {
			case got := <-c.calls:
				t.Fatalf("core was called with %+v, want no call", got)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestMalformedPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "not json", body: "not json at all"},
		{name: "truncated json", body: `{"event_type":"message.received"`},
		{name: "empty body", body: ""},
		{name: "wrong field type", body: `{"event_type":"message.received","contact":{"id":"12345"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := capture{calls: make(chan call, 1)}
			got := post(t, &webhook{secret: secret, core: c, client: &client{}}, tc.body, sign(tc.body, secret)).Code
			if got != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
			}
		})
	}
}

// incoming builds a message.received payload. A zero assignee leaves the
// conversation unassigned, the way respond.io reports one nobody owns.
func incoming(assignee int64) string {
	owner := ""
	if assignee != 0 {
		owner = `,"assignee":{"id":` + strconv.FormatInt(assignee, 10) + `}`
	}
	return `{"event_type":"message.received","contact":{"id":12345` + owner + `},` +
		`"message":{"message":{"type":"text","text":"hello"}}}`
}

func outgoing(source string) string {
	return `{"event_type":"message.sent","contact":{"id":12345},` +
		`"message":{"source":"` + source + `","message":{"type":"text","text":"hello"}}}`
}

// acted answers the one thing the webhook did with a payload, and fails when it
// did nothing or more than one thing.
func acted(t *testing.T, momoAssignee int64, body string) call {
	t.Helper()
	calls := make(chan call, 2)
	h := &webhook{
		secret:       secret,
		core:         capture{calls: calls},
		client:       &client{},
		recorder:     records{calls: calls},
		momoAssignee: momoAssignee,
	}
	if got := post(t, h, body, sign(body, secret)).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	var got call
	select {
	case got = <-calls:
	case <-time.After(time.Second):
		t.Fatal("nothing acted on the message")
	}
	select {
	case extra := <-calls:
		t.Fatalf("the message was acted on twice, also with %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
	return got
}

// TestAnIncomingMessageOfAHandedOverConversationIsRecorded pins what momo does
// while another respond.io user answers: the contact's text stays in the agent's
// session, and the agent writes nothing.
func TestAnIncomingMessageOfAHandedOverConversationIsRecorded(t *testing.T) {
	got := acted(t, 7, incoming(9))
	want := call{direction: "record user", message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the webhook did %+v, want %+v", got, want)
	}
}

func TestMomoAnswersTheConversationsItOwns(t *testing.T) {
	for name, tc := range map[string]struct {
		momoAssignee int64
		assignee     int64
	}{
		"no momo user configured": {momoAssignee: 0, assignee: 9},
		"unassigned":              {momoAssignee: 7, assignee: 0},
		"assigned to momo":        {momoAssignee: 7, assignee: 7},
	} {
		t.Run(name, func(t *testing.T) {
			if got := acted(t, tc.momoAssignee, incoming(tc.assignee)); got.direction != "received" {
				t.Fatalf("the webhook did %+v, want a turn", got)
			}
		})
	}
}

// TestAnOutgoingMessageIsRecordedBySenderSource pins that only text momo did not
// write reaches the session again: momo's own reply arrives with the api source,
// and the session holds it already.
func TestAnOutgoingMessageIsRecordedBySenderSource(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		want   string
	}{
		"a respond.io user":       {source: "user", want: "record assistant"},
		"a workflow":              {source: "workflow", want: "record assistant"},
		"momo's own reply":        {source: "api", want: "sent"},
		"no source":               {source: "", want: "sent"},
		"a source invented later": {source: "campaign", want: "sent"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := acted(t, 7, outgoing(tc.source)); got.direction != tc.want {
				t.Fatalf("the webhook did %+v, want %q", got, tc.want)
			}
		})
	}
}

func TestNewRoutes(t *testing.T) {
	yaml := func(v any) error {
		s, ok := v.(*settings)
		if !ok {
			t.Fatalf("decoded into %T, want *settings", v)
		}
		s.ReceivedSecret = "a"
		s.SentSecret = "b"
		s.APIToken = "api-token"
		return nil
	}
	c, err := New(context.Background(), yaml, capture{}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := c.Routes()
	if len(got) != 2 || got[0].Path != "/respondio/received" || got[1].Path != "/respondio/sent" {
		t.Fatalf("routes = %+v, want the two default webhook paths", got)
	}
}

func TestNewRequiresItsCredentials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*settings)
	}{
		{name: "nothing configured", apply: func(*settings) {}},
		{name: "only received", apply: func(s *settings) { s.ReceivedSecret = "a"; s.APIToken = "t" }},
		{name: "only sent", apply: func(s *settings) { s.SentSecret = "b"; s.APIToken = "t" }},
		{name: "no api token", apply: func(s *settings) { s.ReceivedSecret = "a"; s.SentSecret = "b" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decode := func(v any) error {
				tc.apply(v.(*settings))
				return nil
			}
			if _, err := New(context.Background(), decode, capture{}, nil); err == nil {
				t.Fatal("New succeeded, want an error about the missing signing keys")
			}
		})
	}
}

// TestAMomoAssigneeIDNeedsSessionHistorySync pins the pair an operator cannot
// break: naming momo's user hands conversations to humans, and without the
// extension momo has nowhere to keep what a human writes.
func TestAMomoAssigneeIDNeedsSessionHistorySync(t *testing.T) {
	decode := func(v any) error {
		s := v.(*settings)
		s.ReceivedSecret, s.SentSecret, s.APIToken = "a", "b", "t"
		s.MomoAssigneeID = 7
		return nil
	}
	if _, err := New(context.Background(), decode, capture{}, nil); err == nil {
		t.Fatal("New succeeded, want an error naming momo_assignee_id")
	}
	if _, err := New(context.Background(), decode, capture{}, records{}); err != nil {
		t.Fatalf("New: %v", err)
	}
}
