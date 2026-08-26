package respondio

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
	"github.com/8monkey-ai/momo/internal/extension/sessionhistorysync"
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

func payload(eventType string) string { return delivered(eventType, "", "") }

// delivered is the webhook body respond.io delivers. assignee and sender are
// written as the members they are, so a test states an omitted one as "".
func delivered(eventType, assignee, sender string) string {
	return `{"event_type":"` + eventType + `","contact":{"id":12345` + assignee + `},` +
		`"message":{"message":{"type":"text","text":"hello"}}` + sender + `}`
}

func assignedTo(id int64) string { return `,"assignee":{"id":` + strconv.FormatInt(id, 10) + `}` }

func sentBy(source string) string { return `,"sender":{"source":"` + source + `"}` }

// recording reports into the same calls channel as the handler, so a test states
// which one of the two the event reached.
type recording struct {
	calls chan call
}

func (r recording) Record(_ context.Context, m core.Message, role sessionhistorysync.Role) error {
	r.calls <- call{direction: "record " + string(role), message: m}
	return nil
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

// TestHandover pins who acts on an event: the agent through the handler, the agent
// session through the recorder, or nothing beyond the log.
func TestHandover(t *testing.T) {
	for _, tc := range []struct {
		name       string
		assigneeID int64
		recording  bool
		body       string
		want       string
	}{
		{name: "incoming while another assignee owns the contact", assigneeID: 7, recording: true,
			body: delivered(eventReceived, assignedTo(99), ""), want: "record user"},
		{name: "incoming assigned to momo", assigneeID: 7, recording: true,
			body: delivered(eventReceived, assignedTo(7), ""), want: "received"},
		{name: "incoming with an omitted assignee", assigneeID: 7, recording: true,
			body: delivered(eventReceived, "", ""), want: "received"},
		{name: "incoming with a null assignee", assigneeID: 7, recording: true,
			body: delivered(eventReceived, `,"assignee":null`, ""), want: "received"},
		{name: "outgoing from a user", assigneeID: 7, recording: true,
			body: delivered(eventSent, assignedTo(99), sentBy("user")), want: "record assistant"},
		{name: "outgoing from a user while momo owns the contact", assigneeID: 7, recording: true,
			body: delivered(eventSent, assignedTo(7), sentBy("user")), want: "record assistant"},
		{name: "outgoing from a workflow", assigneeID: 7, recording: true,
			body: delivered(eventSent, assignedTo(99), sentBy("workflow")), want: "record assistant"},
		{name: "outgoing from a workflow while momo owns the contact", assigneeID: 7, recording: true,
			body: delivered(eventSent, assignedTo(7), sentBy("workflow")), want: "record assistant"},
		{name: "outgoing from the api", assigneeID: 7, recording: true,
			body: delivered(eventSent, assignedTo(99), sentBy("api")), want: "sent"},
		{name: "outgoing with no sender", assigneeID: 7, recording: true,
			body: delivered(eventSent, assignedTo(99), ""), want: "sent"},
		{name: "outgoing from a sender momo does not know", assigneeID: 7, recording: true,
			body: delivered(eventSent, "", sentBy("integration")), want: "sent"},

		{name: "incoming with another assignee and no assignee_id", recording: true,
			body: delivered(eventReceived, assignedTo(99), ""), want: "received"},
		{name: "incoming with no assignee and no assignee_id", recording: true,
			body: delivered(eventReceived, "", ""), want: "received"},
		{name: "outgoing from a user with no assignee_id", recording: true,
			body: delivered(eventSent, "", sentBy("user")), want: "record assistant"},
		{name: "outgoing from a workflow with no assignee_id", recording: true,
			body: delivered(eventSent, "", sentBy("workflow")), want: "record assistant"},
		{name: "outgoing from the api with no assignee_id", recording: true,
			body: delivered(eventSent, "", sentBy("api")), want: "sent"},

		{name: "incoming with another assignee and the extension off",
			body: delivered(eventReceived, assignedTo(99), ""), want: "received"},
		{name: "outgoing from a user with the extension off",
			body: delivered(eventSent, "", sentBy("user")), want: "sent"},
		{name: "outgoing from a workflow with the extension off",
			body: delivered(eventSent, "", sentBy("workflow")), want: "sent"},
		{name: "outgoing from the api with the extension off",
			body: delivered(eventSent, "", sentBy("api")), want: "sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := make(chan call, 2)
			h := &webhook{secret: secret, core: capture{calls: calls}, client: &client{}, assigneeID: tc.assigneeID}
			if tc.recording {
				h.recorder = recording{calls: calls}
			}
			if got := post(t, h, tc.body, sign(tc.body, secret)).Code; got != http.StatusOK {
				t.Fatalf("status = %d, want %d", got, http.StatusOK)
			}
			select {
			case got := <-calls:
				want := call{direction: tc.want, message: core.Message{Conversation: "12345", Content: core.Text("hello")}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("the event reached %+v, want %+v", got, want)
				}
			case <-time.After(time.Second):
				t.Fatalf("nothing was called, want %s", tc.want)
			}
			select {
			case extra := <-calls:
				t.Fatalf("the event reached %+v as well, want only %s", extra, tc.want)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

type answering struct{}

func (answering) Received(ctx context.Context, _ core.Message, reply core.Reply) error {
	return reply(ctx, core.Text("the answer"))
}

func (answering) Sent(context.Context, core.Message) {}

func TestAnIncomingMessageMomoOwnsIsAnswered(t *testing.T) {
	sent := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the API call: %v", err)
			return
		}
		sent <- r.URL.Path + " " + string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	body := delivered(eventReceived, assignedTo(7), "")
	h := &webhook{
		secret: secret, core: answering{}, assigneeID: 7,
		recorder: recording{calls: make(chan call, 1)},
		client:   &client{url: api.URL, token: "token", http: api.Client()},
	}
	if got := post(t, h, body, sign(body, secret)).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	want := `/contact/id:12345/message {"message":{"text":"the answer","type":"text"}}`
	select {
	case got := <-sent:
		if got != want {
			t.Fatalf("respond.io received %s, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("no reply reached respond.io, want %s", want)
	}
}

func TestNewRefusesAnAssigneeWithoutTheExtension(t *testing.T) {
	decode := func(v any) error {
		s := v.(*settings)
		s.ReceivedSecret = "a"
		s.SentSecret = "b"
		s.APIToken = "token"
		s.AssigneeID = 7
		return nil
	}
	if _, err := New(context.Background(), decode, capture{}, nil); err == nil {
		t.Fatal("New succeeded, want an error about the handover momo cannot record")
	}
	if _, err := New(context.Background(), decode, capture{}, recording{}); err != nil {
		t.Fatalf("New with the extension enabled: %v", err)
	}
}
