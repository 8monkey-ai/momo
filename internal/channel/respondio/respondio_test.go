package respondio

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
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

func (c capture) Received(ctx context.Context, m core.Message, _ core.Reply) {
	c.calls <- call{direction: "received", message: m, ctxErr: ctx.Err()}
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

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// apiCall is what the fake respond.io API saw of one reply.
type apiCall struct {
	method      string
	path        string
	auth        string
	contentType string
	body        string
}

// fakeAPI stands in for respond.io's REST API, answering every call with status.
func fakeAPI(t *testing.T, status int, body string) (*client, chan apiCall) {
	t.Helper()
	calls := make(chan apiCall, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ := io.ReadAll(r.Body)
		calls <- apiCall{
			method:      r.Method,
			path:        r.URL.Path,
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			body:        string(sent),
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &client{http: srv.Client(), url: srv.URL, token: "api-token"}, calls
}

func nextCall(t *testing.T, calls chan apiCall) apiCall {
	t.Helper()
	select {
	case got := <-calls:
		return got
	case <-time.After(time.Second):
		t.Fatal("the API was never called")
		return apiCall{}
	}
}

func noCall(t *testing.T, calls chan apiCall) {
	t.Helper()
	select {
	case got := <-calls:
		t.Fatalf("the API was called with %+v, want no call", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func payload(eventType string) string { return payloadFor(eventType, 12345, "hello") }

func payloadFor(eventType string, contactID int64, text string) string {
	return `{"event_type":"` + eventType + `","contact":{"id":` + strconv.FormatInt(contactID, 10) + `},` +
		`"message":{"message":{"type":"text","text":"` + text + `"}}}`
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
			if got := post(t, &webhook{secret: secret, core: c}, body, tc.signature).Code; got != tc.status {
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
			if got := post(t, &webhook{secret: secret, core: c}, tc.body, sign(tc.body, secret)).Code; got != http.StatusOK {
				t.Fatalf("status = %d, want %d", got, http.StatusOK)
			}
			select {
			case got := <-c.calls:
				want := call{direction: tc.direction, message: core.Message{Contact: "12345", Content: core.Text("hello")}}
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
	(&webhook{secret: secret, core: c}).ServeHTTP(httptest.NewRecorder(), r)

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
			if got := post(t, &webhook{secret: secret, core: c}, tc.body, sign(tc.body, secret)).Code; got != http.StatusOK {
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
			got := post(t, &webhook{secret: secret, core: c}, tc.body, sign(tc.body, secret)).Code
			if got != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
			}
		})
	}
}

func TestNewRoutes(t *testing.T) {
	c, err := New(context.Background(), configured(t, func(*settings) {}), capture{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := c.Routes()
	if len(got) != 2 || got[0].Path != "/respondio/received" || got[1].Path != "/respondio/sent" {
		t.Fatalf("routes = %+v, want the two default webhook paths", got)
	}
}

// configured decodes a usable configuration, with apply changing what a test is
// about.
func configured(t *testing.T, apply func(*settings)) channel.Decoder {
	return func(v any) error {
		s, ok := v.(*settings)
		if !ok {
			t.Fatalf("decoded into %T, want *settings", v)
		}
		s.ReceivedSecret = "a"
		s.SentSecret = "b"
		s.APIToken = "api-token"
		apply(s)
		return nil
	}
}

func TestNewRequiresAnAPIToken(t *testing.T) {
	decode := configured(t, func(s *settings) { s.APIToken = "" })
	if _, err := New(context.Background(), decode, capture{}); err == nil {
		t.Fatal("New succeeded, want an error about the missing api_token")
	}
}

func TestNewDefaultsTheAPIURL(t *testing.T) {
	c, err := New(context.Background(), configured(t, func(*settings) {}), capture{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := c.Routes()[0].Handler.(*webhook).api.url
	if got != "https://api.respond.io/v2" {
		t.Fatalf("api url = %q, want \"https://api.respond.io/v2\"", got)
	}
}

func TestReplyGoesToTheContactsMessageEndpoint(t *testing.T) {
	api, calls := fakeAPI(t, http.StatusOK, `{"messageId":1}`)
	if err := api.send(context.Background(), "12345", "hello"); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := nextCall(t, calls)
	want := apiCall{
		method:      "POST",
		path:        "/contact/id:12345/message",
		auth:        "Bearer api-token",
		contentType: "application/json",
		body:        `{"message":{"type":"text","text":"hello"}}`,
	}
	if got != want {
		t.Fatalf("the API saw %+v, want %+v", got, want)
	}
}

func TestNothingToSayIssuesNoCall(t *testing.T) {
	api, calls := fakeAPI(t, http.StatusOK, `{}`)
	if err := api.send(context.Background(), "12345", ""); err != nil {
		t.Fatalf("send: %v", err)
	}
	noCall(t, calls)
}

func TestReplyErrorCarriesTheStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			api, _ := fakeAPI(t, status, `{"message":"refused"}`)
			err := api.send(context.Background(), "12345", "hello")
			if err == nil {
				t.Fatal("send succeeded, want an error naming the status")
			}
			if !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Fatalf("error %q does not name status %d", err, status)
			}
			if !strings.Contains(err.Error(), "refused") {
				t.Fatalf("error %q does not carry the response body", err)
			}
		})
	}
}

func TestEchoAnswersAnIncomingMessageAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		call  bool
	}{
		{name: "incoming message is answered", event: eventReceived, call: true},
		{name: "outgoing message is not", event: eventSent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, calls := fakeAPI(t, http.StatusOK, `{}`)
			h := &webhook{secret: secret, core: core.EchoHandler{Log: discard()}, api: api}
			body := payload(tc.event)
			if got := post(t, h, body, sign(body, secret)).Code; got != http.StatusOK {
				t.Fatalf("status = %d, want %d", got, http.StatusOK)
			}
			if !tc.call {
				noCall(t, calls)
				return
			}
			if got := nextCall(t, calls); got.body != `{"message":{"type":"text","text":"hello"}}` {
				t.Fatalf("body = %s, want the text that arrived", got.body)
			}
			noCall(t, calls)
		})
	}
}

func TestConcurrentWebhooksAnswerTheirOwnContact(t *testing.T) {
	api, calls := fakeAPI(t, http.StatusOK, `{}`)
	h := &webhook{secret: secret, core: core.EchoHandler{Log: discard()}, api: api}
	for _, contactID := range []int64{111, 222} {
		body := payloadFor(eventReceived, contactID, "hello")
		go post(t, h, body, sign(body, secret))
	}
	paths := []string{nextCall(t, calls).path, nextCall(t, calls).path}
	slices.Sort(paths)
	want := []string{"/contact/id:111/message", "/contact/id:222/message"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("the API saw %v, want %v", paths, want)
	}
	noCall(t, calls)
}

func TestNewRequiresBothSecrets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*settings)
	}{
		{name: "no secrets", apply: func(*settings) {}},
		{name: "only received", apply: func(s *settings) { s.ReceivedSecret = "a" }},
		{name: "only sent", apply: func(s *settings) { s.SentSecret = "b" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decode := func(v any) error {
				s := v.(*settings)
				s.APIToken = "api-token"
				tc.apply(s)
				return nil
			}
			if _, err := New(context.Background(), decode, capture{}); err == nil {
				t.Fatal("New succeeded, want an error about the missing signing keys")
			}
		})
	}
}
