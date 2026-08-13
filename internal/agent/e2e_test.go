package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/channel/respondio"
	"github.com/8monkey-ai/momo/internal/core"
)

const secret = "signing-key"

// apiCall is what reached the stand-in for the respond.io API.
type apiCall struct {
	path string
	body string
}

// respondIO serves the channel and the API it answers on, so a test can drive one
// message from the webhook to the call the reply makes.
type respondIO struct {
	webhook http.Handler
	calls   chan apiCall
}

func newRespondIO(t *testing.T, a *Agent) *respondIO {
	t.Helper()
	r := &respondIO{calls: make(chan apiCall, 4)}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.calls <- apiCall{path: req.URL.Path, body: string(body)}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(api.Close)

	decode := func(v any) error {
		return yamlDecoder("received_secret: " + secret + "\nsent_secret: " + secret +
			"\napi_token: api-token\napi_url: " + api.URL + "\n")(v)
	}
	handler := core.Turn{Agent: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c, err := respondio.New(context.Background(), channel.Decoder(decode), handler)
	if err != nil {
		t.Fatalf("respondio.New: %v", err)
	}
	r.webhook = c.Routes()[0].Handler
	return r
}

// receive delivers a signed incoming-message webhook.
func (r *respondIO) receive(t *testing.T, text string) {
	t.Helper()
	body := `{"event_type":"message.received","contact":{"id":12345},` +
		`"message":{"message":{"type":"text","text":"` + text + `"}}}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/respondio/received", strings.NewReader(body))
	req.Header.Set("X-Webhook-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	r.webhook.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook = %d, want %d", w.Code, http.StatusOK)
	}
}

func (r *respondIO) next(t *testing.T) apiCall {
	t.Helper()
	select {
	case call := <-r.calls:
		return call
	case <-time.After(30 * time.Second):
		t.Fatal("the API was never called")
		return apiCall{}
	}
}

func (r *respondIO) silent(t *testing.T) {
	t.Helper()
	select {
	case call := <-r.calls:
		t.Fatalf("the API was called with %+v, want no further call", call)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestAMessageOnAChannelIsAnsweredByTheHarness runs the whole path: a webhook
// arrives, the harness receives the prompt, and the reply leaves on the channel
// that received the message.
func TestAMessageOnAChannelIsAnsweredByTheHarness(t *testing.T) {
	r := newRespondIO(t, newAgent(t, "", time.Minute))

	r.receive(t, "hello")

	call := r.next(t)
	if call.path != "/contact/id:12345/message" {
		t.Fatalf("the API call went to %q, want the message endpoint", call.path)
	}
	if !strings.Contains(call.body, "turn 1: hello") {
		t.Fatalf("the message body is %s, want the reply of the harness", call.body)
	}
	r.silent(t)
}

// TestAFailedTurnBecomesACommentOnTheConversation pins what respond.io learns
// from a turn that produced no reply: the workspace sees a comment, and the
// contact sees no message.
func TestAFailedTurnBecomesACommentOnTheConversation(t *testing.T) {
	r := newRespondIO(t, newAgent(t, "fail", time.Minute))

	r.receive(t, "hello")

	call := r.next(t)
	if call.path != "/contact/id:12345/comment" {
		t.Fatalf("the API call went to %q, want the comment endpoint", call.path)
	}
	if !strings.Contains(call.body, "momo did not answer") {
		t.Fatalf("the comment body is %s, want momo's own text", call.body)
	}
	r.silent(t)
}
