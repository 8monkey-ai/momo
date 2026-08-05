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
	message   core.Message
	ctxErr    error
}

type capture struct {
	calls chan call
}

func (c capture) Received(ctx context.Context, m core.Message) {
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
				want := call{
					direction: tc.direction,
					message:   core.Message{Contact: "12345", Content: []core.Block{core.Text("hello")}},
				}
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
	yaml := func(v any) error {
		s, ok := v.(*settings)
		if !ok {
			t.Fatalf("decoded into %T, want *settings", v)
		}
		s.ReceivedSecret = "a"
		s.SentSecret = "b"
		return nil
	}
	c, err := New(t.Context(), yaml, capture{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := c.Routes()
	if len(got) != 2 || got[0].Path != "/respondio/received" || got[1].Path != "/respondio/sent" {
		t.Fatalf("routes = %+v, want the two default webhook paths", got)
	}
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
				tc.apply(v.(*settings))
				return nil
			}
			if _, err := New(t.Context(), decode, capture{}); err == nil {
				t.Fatal("New succeeded, want an error about the missing signing keys")
			}
		})
	}
}
