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
	handler   string
	direction string
	message   core.Message
	ctxErr    error
}

type capture struct {
	name  string
	calls chan call
}

func (c capture) Received(ctx context.Context, m core.Message, _ core.Reply) error {
	c.calls <- call{handler: c.name, direction: "received", message: m, ctxErr: ctx.Err()}
	return nil
}

func (c capture) Sent(ctx context.Context, m core.Message) {
	c.calls <- call{handler: c.name, direction: "sent", message: m, ctxErr: ctx.Err()}
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

// incoming is the payload of a received message whose contact is assigned to the
// named respond.io user. respond.io sends a conversation nobody owns as a null.
func incoming(assignee int64) string {
	who := "null"
	if assignee != 0 {
		who = `{"id":` + strconv.FormatInt(assignee, 10) + `,"email":"operator@example.com"}`
	}
	return `{"event_type":"message.received","contact":{"id":12345,"assignee":` + who + `},` +
		`"message":{"message":{"type":"text","text":"hello"}}}`
}

// outgoing is the payload of a sent message from the named sender source. An empty
// source leaves the sender block out.
func outgoing(source string) string {
	sender := ""
	if source != "" {
		sender = `,"sender":{"source":"` + source + `","userId":2}`
	}
	return `{"event_type":"message.sent","contact":{"id":12345,"assignee":{"id":2}},` +
		`"message":{"message":{"type":"text","text":"hello"}}` + sender + `}`
}

// routing answers the one handler call a payload leads to.
func routing(t *testing.T, momoUserID int64, withHistory bool, body string) call {
	t.Helper()
	calls := make(chan call, 2)
	w := &webhook{secret: secret, momoUserID: momoUserID, core: capture{name: "core", calls: calls}, client: &client{}}
	if withHistory {
		w.history = capture{name: "history", calls: calls}
	}
	if got := post(t, w, body, sign(body, secret)).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	var got call
	select {
	case got = <-calls:
	case <-time.After(time.Second):
		t.Fatal("no handler was called")
	}
	// One payload reaches one handler: a message both answered and recorded would
	// reach the agent twice.
	select {
	case extra := <-calls:
		t.Fatalf("a second handler was called: %+v", extra)
	case <-time.After(10 * time.Millisecond):
	}
	return got
}

func TestWhoOwnsAnIncomingMessage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		momoUserID int64
		body       string
		want       string
	}{
		{name: "another assignee owns the conversation", momoUserID: 7, body: incoming(9), want: "history"},
		{name: "momo is the assignee", momoUserID: 7, body: incoming(7), want: "core"},
		{name: "nobody is the assignee", momoUserID: 7, body: incoming(0), want: "core"},
		{name: "the payload carries no assignee", momoUserID: 7, body: payload(eventReceived), want: "core"},
		{name: "no momo user id is configured", momoUserID: 0, body: incoming(9), want: "core"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := routing(t, tc.momoUserID, true, tc.body)
			want := call{handler: tc.want, direction: "received",
				message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("call = %+v, want %+v", got, want)
			}
		})
	}
}

func TestWhichOutgoingMessageIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "an operator wrote it", source: "user", want: "history"},
		{name: "a workflow sent it", source: "workflow", want: "history"},
		{name: "momo sent it through the API", source: "api", want: "core"},
		{name: "a source momo does not know", source: "bot", want: "core"},
		{name: "no sender block", source: "", want: "core"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := routing(t, 7, true, outgoing(tc.source))
			want := call{handler: tc.want, direction: "sent",
				message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("call = %+v, want %+v", got, want)
			}
		})
	}
}

func TestWithoutTheExtensionNothingIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		direction string
	}{
		{name: "an incoming message", body: incoming(9), direction: "received"},
		{name: "an operator's message", body: outgoing("user"), direction: "sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := routing(t, 0, false, tc.body)
			want := call{handler: "core", direction: tc.direction,
				message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("call = %+v, want %+v", got, want)
			}
		})
	}
}

// momo refuses the configuration rather than losing the messages of another
// assignee in silence.
func TestAMomoUserIDRequiresTheHistoryExtension(t *testing.T) {
	for _, tc := range []struct {
		name       string
		momoUserID int64
		history    core.Handler
		wantError  bool
	}{
		{name: "a user id without the extension", momoUserID: 7, wantError: true},
		{name: "a user id with the extension", momoUserID: 7, history: capture{}},
		{name: "no user id and no extension", momoUserID: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decode := func(v any) error {
				s := v.(*settings)
				s.ReceivedSecret = "a"
				s.SentSecret = "b"
				s.APIToken = "t"
				s.MomoUserID = tc.momoUserID
				return nil
			}
			_, err := New(context.Background(), decode, capture{}, tc.history)
			if tc.wantError && err == nil {
				t.Fatal("New succeeded, want an error naming the session history extension")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("New: %v", err)
			}
		})
	}
}

type replyCapture struct{ replies chan core.Reply }

func (c replyCapture) Received(_ context.Context, _ core.Message, reply core.Reply) error {
	c.replies <- reply
	return nil
}

func (c replyCapture) Sent(context.Context, core.Message) {}

// The operator owns the conversation, so a record is given no reply: momo must not
// be able to write into it.
func TestARecordedMessageCannotAnswerTheContact(t *testing.T) {
	replies := make(chan core.Reply, 1)
	w := &webhook{secret: secret, momoUserID: 7,
		core:    capture{name: "core", calls: make(chan call, 1)},
		history: replyCapture{replies: replies},
		client:  &client{}}
	body := incoming(9)
	if got := post(t, w, body, sign(body, secret)).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	select {
	case reply := <-replies:
		if reply != nil {
			t.Fatal("the record was given a reply, want none")
		}
	case <-time.After(time.Second):
		t.Fatal("the history handler was not called")
	}
}
