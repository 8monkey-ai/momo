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

func (c capture) RecordUser(ctx context.Context, m core.Message) {
	c.calls <- call{direction: "recorded user", message: m, ctxErr: ctx.Err()}
}

func (c capture) RecordAssistant(ctx context.Context, m core.Message) {
	c.calls <- call{direction: "recorded assistant", message: m, ctxErr: ctx.Err()}
}

// incoming is a message.received event of a conversation assigned to assigneeID,
// and of an unassigned one when it is zero.
func incoming(assigneeID int64) string {
	assignee := "null"
	if assigneeID != 0 {
		assignee = `{"id":` + strconv.FormatInt(assigneeID, 10) + `}`
	}
	return `{"event_type":"` + eventReceived + `","contact":{"id":12345,"assignee":` + assignee + `},` +
		`"message":{"message":{"type":"text","text":"hello"}}}`
}

// outgoing is a message.sent event the named sender source produced.
func outgoing(source string) string {
	return `{"event_type":"` + eventSent + `","contact":{"id":12345},"sender":{"source":"` + source + `"},` +
		`"message":{"message":{"type":"text","text":"hello"}}}`
}

// direction answers what the webhook did with one event, and "nothing" when it
// acted on none.
func direction(t *testing.T, h *webhook, body string) string {
	t.Helper()
	if got := post(t, h, body, sign(body, secret)).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	select {
	case got := <-h.core.(capture).calls:
		if want := (core.Message{Conversation: "12345", Content: core.Text("hello")}); !reflect.DeepEqual(got.message, want) {
			t.Fatalf("the message was %+v, want %+v", got.message, want)
		}
		return got.direction
	case <-time.After(time.Second):
		return "nothing"
	}
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

// TestMomoOwnsAConversationNobodyElseIsAssignedTo pins which incoming messages
// momo answers and which ones it only records: the conversation another assignee
// owns is answered by that human, and the agent reads it on its next turn.
func TestMomoOwnsAConversationNobodyElseIsAssignedTo(t *testing.T) {
	for name, tc := range map[string]struct {
		momoAssigneeID int64
		assigneeID     int64
		want           string
	}{
		"no momo assignee, unassigned":        {want: "received"},
		"no momo assignee, another human":     {assigneeID: 77, want: "received"},
		"momo assigned":                       {momoAssigneeID: 42, assigneeID: 42, want: "received"},
		"unassigned":                          {momoAssigneeID: 42, want: "received"},
		"another human owns the conversation": {momoAssigneeID: 42, assigneeID: 77, want: "recorded user"},
	} {
		t.Run(name, func(t *testing.T) {
			c := capture{calls: make(chan call, 2)}
			h := &webhook{secret: secret, core: c, history: c, client: &client{}, momoAssigneeID: tc.momoAssigneeID}
			if got := direction(t, h, incoming(tc.assigneeID)); got != tc.want {
				t.Fatalf("the webhook %s the message, want %s", got, tc.want)
			}
			select {
			case got := <-c.calls:
				t.Fatalf("the webhook also %s the message, want one action only", got.direction)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// TestAnOutgoingMessageOfAHumanOrAWorkflowIsRecorded pins what momo does with the
// replies it did not write: they become assistant messages of the session, while
// momo's own replies are already in it and are only logged.
func TestAnOutgoingMessageOfAHumanOrAWorkflowIsRecorded(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		want   string
	}{
		"a respond.io user": {source: "user", want: "recorded assistant"},
		"a workflow":        {source: "workflow", want: "recorded assistant"},
		"momo's own reply":  {source: "api", want: "sent"},
		"no source":         {source: "", want: "sent"},
	} {
		t.Run(name, func(t *testing.T) {
			c := capture{calls: make(chan call, 2)}
			h := &webhook{secret: secret, core: c, history: c, client: &client{}}
			if got := direction(t, h, outgoing(tc.source)); got != tc.want {
				t.Fatalf("the webhook %s the message, want %s", got, tc.want)
			}
		})
	}
}

// TestWithoutHistorySyncEveryMessageIsProcessedNormally pins that a deployment
// without the extension keeps working: nothing is recorded, and the human's reply
// is logged as it was before.
func TestWithoutHistorySyncEveryMessageIsProcessedNormally(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"an incoming message":             {body: incoming(77), want: "received"},
		"the outgoing message of a human": {body: outgoing("user"), want: "sent"},
	} {
		t.Run(name, func(t *testing.T) {
			c := capture{calls: make(chan call, 2)}
			h := &webhook{secret: secret, core: c, client: &client{}}
			if got := direction(t, h, tc.body); got != tc.want {
				t.Fatalf("the webhook %s the message, want %s", got, tc.want)
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

// TestAMomoAssigneeIDNeedsHistorySync pins the configuration momo refuses to
// serve: without the extension a conversation another assignee owns would reach
// nobody, so the assignee id is only accepted with it.
func TestAMomoAssigneeIDNeedsHistorySync(t *testing.T) {
	for name, tc := range map[string]struct {
		momoAssigneeID int64
		history        core.History
		valid          bool
	}{
		"an assignee id and the extension":     {momoAssigneeID: 42, history: capture{}, valid: true},
		"no assignee id and no extension":      {valid: true},
		"an assignee id without the extension": {momoAssigneeID: 42},
		"a negative assignee id":               {momoAssigneeID: -1, history: capture{}},
	} {
		t.Run(name, func(t *testing.T) {
			decode := func(v any) error {
				s := v.(*settings)
				s.ReceivedSecret, s.SentSecret, s.APIToken = "a", "b", "t"
				s.MomoAssigneeID = tc.momoAssigneeID
				return nil
			}
			_, err := New(context.Background(), decode, capture{}, tc.history)
			if tc.valid && err != nil {
				t.Fatalf("New: %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("New succeeded, want an error naming momo_assignee_id")
			}
		})
	}
}
