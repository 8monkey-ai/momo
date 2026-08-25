package respondio

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

const secret = "signing-key"

type call struct {
	direction string
	role      core.Role
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

func (c capture) Record(ctx context.Context, m core.Message, role core.Role) {
	c.calls <- call{direction: "recorded", role: role, message: m, ctxErr: ctx.Err()}
}

func sign(body, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func post(t *testing.T, h http.Handler, payload, signature string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/respondio/received", strings.NewReader(payload))
	if signature != "" {
		r.Header.Set(signatureHeader, signature)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func payload(eventType string) string {
	return body(eventType, "", "")
}

// assignee and sender are raw JSON. An empty value omits the member.
func body(eventType, assignee, sender string) string {
	contact := `"contact":{"id":12345`
	if assignee != "" {
		contact += `,"assignee":` + assignee
	}
	contact += `}`
	if sender != "" {
		sender = `,"sender":` + sender
	}
	return `{"event_type":"` + eventType + `",` + contact + sender +
		`,"message":{"message":{"type":"text","text":"hello"}}}`
}

// TestHandoverRoutesAnEventByOwnerAndSender pins who each event belongs to: an
// incoming message goes by the owner of the contact, an outgoing one by its sender.
func TestHandoverRoutesAnEventByOwnerAndSender(t *testing.T) {
	for name, tc := range map[string]struct {
		assigneeID int64
		body       string
		want       call
	}{
		"another assignee holds the contact": {
			assigneeID: 7,
			body:       body(eventReceived, `{"id":99}`, ""),
			want:       call{direction: "recorded", role: core.Role("user")},
		},
		"momo holds the contact": {
			assigneeID: 7,
			body:       body(eventReceived, `{"id":7}`, ""),
			want:       call{direction: "received"},
		},
		"the contact has no assignee": {
			assigneeID: 7,
			body:       body(eventReceived, "", ""),
			want:       call{direction: "received"},
		},
		"the assignee is null": {
			assigneeID: 7,
			body:       body(eventReceived, "null", ""),
			want:       call{direction: "received"},
		},
		"an operator sent the message": {
			assigneeID: 7,
			body:       body(eventSent, `{"id":99}`, `{"source":"user"}`),
			want:       call{direction: "recorded", role: core.Role("assistant")},
		},
		"a workflow sent the message": {
			assigneeID: 7,
			body:       body(eventSent, `{"id":7}`, `{"source":"workflow"}`),
			want:       call{direction: "recorded", role: core.Role("assistant")},
		},
		"momo sent the message through the API": {
			assigneeID: 7,
			body:       body(eventSent, `{"id":7}`, `{"source":"api"}`),
			want:       call{direction: "sent"},
		},
		"the outgoing message has no sender": {
			assigneeID: 7,
			body:       body(eventSent, `{"id":7}`, ""),
			want:       call{direction: "sent"},
		},
		"the sender source is unknown": {
			assigneeID: 7,
			body:       body(eventSent, `{"id":7}`, `{"source":"invented.later"}`),
			want:       call{direction: "sent"},
		},
		"no assignee_id and another assignee": {
			body: body(eventReceived, `{"id":99}`, ""),
			want: call{direction: "received"},
		},
		"no assignee_id and no assignee": {
			body: body(eventReceived, "", ""),
			want: call{direction: "received"},
		},
		"no assignee_id and an operator's message": {
			body: body(eventSent, `{"id":99}`, `{"source":"user"}`),
			want: call{direction: "recorded", role: core.Role("assistant")},
		},
		"no assignee_id and a workflow's message": {
			body: body(eventSent, "", `{"source":"workflow"}`),
			want: call{direction: "recorded", role: core.Role("assistant")},
		},
		"no assignee_id and momo's own message": {
			body: body(eventSent, "", `{"source":"api"}`),
			want: call{direction: "sent"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := capture{calls: make(chan call, 1)}
			h := &webhook{secret: secret, core: c, client: &client{}, assigneeID: tc.assigneeID}
			post(t, h, tc.body, sign(tc.body, secret))

			want := tc.want
			want.message = core.Message{Conversation: "12345", Content: []core.ContentBlock{{Type: "text", Text: "hello"}}}
			select {
			case got := <-c.calls:
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("core called with %+v, want %+v", got, want)
				}
			case <-time.After(time.Second):
				t.Fatalf("core was never called, want %+v", want)
			}
		})
	}
}

// TestOnlyTheOwnerOfTheContactGetsAnAgentAnswer pins what the contact reads: momo
// answers the message it owns, and says nothing while another assignee holds the
// conversation.
func TestOnlyTheOwnerOfTheContactGetsAnAgentAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		assignee string
		answers  bool
	}{
		"momo holds the contact":             {assignee: `{"id":7}`, answers: true},
		"the contact has no assignee":        {assignee: "", answers: true},
		"another assignee holds the contact": {assignee: `{"id":99}`},
	} {
		t.Run(name, func(t *testing.T) {
			a := newAPI(t)
			h := &webhook{secret: secret, core: echoHandler(), client: a.client(), assigneeID: 7}
			event := body(eventReceived, tc.assignee, "")
			post(t, h, event, sign(event, secret))

			if !tc.answers {
				a.silent(t)
				return
			}
			got := a.next(t)
			if got.path != "/contact/id:12345/message" || got.body != `{"message":{"text":"hello","type":"text"}}` {
				t.Fatalf("API call = %+v, want the answer to contact 12345", got)
			}
			a.silent(t)
		})
	}
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
				want := call{direction: tc.direction, message: core.Message{Conversation: "12345", Content: []core.ContentBlock{{Type: "text", Text: "hello"}}}}
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
	c, err := New(context.Background(), yaml, capture{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := c.Routes()
	if len(got) != 2 || got[0].Path != "/respondio/received" || got[1].Path != "/respondio/sent" {
		t.Fatalf("routes = %+v, want the two default webhook paths", got)
	}
}

func TestNewGivesBothWebhooksTheAssignee(t *testing.T) {
	decode := func(v any) error {
		s := v.(*settings)
		s.ReceivedSecret = "a"
		s.SentSecret = "b"
		s.APIToken = "api-token"
		s.AssigneeID = 7
		return nil
	}
	c, err := New(context.Background(), decode, capture{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, route := range c.Routes() {
		h, ok := route.Handler.(*webhook)
		if !ok {
			t.Fatalf("route %s is served by %T, want *webhook", route.Path, route.Handler)
		}
		if h.assigneeID != 7 {
			t.Fatalf("route %s has assigneeID %d, want 7", route.Path, h.assigneeID)
		}
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
			if _, err := New(context.Background(), decode, capture{}); err == nil {
				t.Fatal("New succeeded, want an error about the missing signing keys")
			}
		})
	}
}
