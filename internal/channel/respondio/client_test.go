package respondio

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

// echoAgent answers with the content the message carried, so a test drives the
// reply path with no agent subprocess.
type echoAgent struct{}

func (echoAgent) Turn(_ context.Context, m core.Message, emit core.Emit) error {
	for _, block := range m.Content {
		if err := emit([]core.ContentBlock{block}); err != nil {
			return err
		}
	}
	return nil
}

func echoHandler() core.Handler {
	return core.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), echoAgent{})
}

// failingAgent stands for an agent that exits before it replies.
type failingAgent struct{}

func (failingAgent) Turn(context.Context, core.Message, core.Emit) error {
	return errors.New("the agent exited before it replied")
}

func failingHandler() core.Handler {
	return core.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), failingAgent{})
}

// api is a stand-in for respond.io's REST API, recording what reached it.
type api struct {
	url      string
	status   int
	body     string
	requests chan apiRequest
}

type apiRequest struct {
	method        string
	path          string
	authorization string
	contentType   string
	body          string
}

func newAPI(t *testing.T) *api {
	t.Helper()
	a := &api{status: http.StatusOK, requests: make(chan apiRequest, 16)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.requests <- apiRequest{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
			body:          string(body),
		}
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	a.url = srv.URL
	return a
}

func (a *api) client() *client {
	return &client{url: a.url, token: "api-token", http: &http.Client{Timeout: 2 * time.Second}}
}

// next waits for the call the asynchronous dispatch makes.
func (a *api) next(t *testing.T) apiRequest {
	t.Helper()
	select {
	case req := <-a.requests:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("the API was never called")
		return apiRequest{}
	}
}

func (a *api) silent(t *testing.T) {
	t.Helper()
	select {
	case req := <-a.requests:
		t.Fatalf("the API was called with %+v, want no call", req)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReplySendsATextMessageToTheContact(t *testing.T) {
	a := newAPI(t)
	if err := a.client().reply("12345")(context.Background(), core.Text("hello")); err != nil {
		t.Fatalf("reply: %v", err)
	}
	got := a.next(t)
	want := apiRequest{
		method:        "POST",
		path:          "/contact/id:12345/message",
		authorization: "Bearer api-token",
		contentType:   "application/json",
		body:          `{"message":{"text":"hello","type":"text"}}`,
	}
	if got != want {
		t.Fatalf("API call = %+v, want %+v", got, want)
	}
}

func TestReplySendsTypedAttachments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block core.ContentBlock
		want  string
	}{
		{name: "image MIME", block: core.ContentBlock{Type: "image", URI: "https://files.example/photo", MimeType: "image/png"}, want: `{"message":{"attachment":{"type":"image","url":"https://files.example/photo"},"type":"attachment"}}`},
		{name: "resource link image", block: core.ContentBlock{Type: "resource_link", URI: "https://files.example/photo", MimeType: "image/webp"}, want: `{"message":{"attachment":{"type":"image","url":"https://files.example/photo"},"type":"attachment"}}`},
		{name: "resource link video", block: core.ContentBlock{Type: "resource_link", URI: "https://files.example/movie", MimeType: "video/mp4"}, want: `{"message":{"attachment":{"type":"video","url":"https://files.example/movie"},"type":"attachment"}}`},
		{name: "resource link audio", block: core.ContentBlock{Type: "resource_link", URI: "https://files.example/sound", MimeType: "audio/mpeg"}, want: `{"message":{"attachment":{"type":"audio","url":"https://files.example/sound"},"type":"attachment"}}`},
		{name: "resource link file", block: core.ContentBlock{Type: "resource_link", URI: "https://files.example/report", MimeType: "application/pdf"}, want: `{"message":{"attachment":{"type":"file","url":"https://files.example/report"},"type":"attachment"}}`},
		{name: "embedded resource", block: core.ContentBlock{Type: "resource", Resource: &core.Resource{URI: "https://files.example/movie", MimeType: "video/mp4"}}, want: `{"message":{"attachment":{"type":"video","url":"https://files.example/movie"},"type":"attachment"}}`},
		{name: "extension fallback", block: core.ContentBlock{Type: "resource_link", URI: "https://files.example/PHOTO.PNG?download=1#view"}, want: `{"message":{"attachment":{"type":"image","url":"https://files.example/PHOTO.PNG?download=1#view"},"type":"attachment"}}`},
		{name: "unknown extension fallback", block: core.ContentBlock{Type: "resource_link", URI: "https://files.example/archive.unknownext"}, want: `{"message":{"attachment":{"type":"file","url":"https://files.example/archive.unknownext"},"type":"attachment"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAPI(t)
			if err := a.client().reply("7")(context.Background(), []core.ContentBlock{tc.block}); err != nil {
				t.Fatalf("reply: %v", err)
			}
			if got := a.next(t).body; got != tc.want {
				t.Fatalf("body = %s, want %s", got, tc.want)
			}
			a.silent(t)
		})
	}
}

func TestReplyRejectsBlocksWithoutAUsableURL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block core.ContentBlock
	}{
		{name: "audio", block: core.ContentBlock{Type: "audio", Data: "AAAA", MimeType: "audio/mpeg"}},
		{name: "data-only image", block: core.ContentBlock{Type: "image", Data: "AAAA", MimeType: "image/png"}},
		{name: "resource link", block: core.ContentBlock{Type: "resource_link", URI: "file:///private/report.pdf", MimeType: "application/pdf"}},
		{name: "embedded resource", block: core.ContentBlock{Type: "resource", Resource: &core.Resource{URI: "/report.pdf", MimeType: "application/pdf"}}},
		{name: "nil resource", block: core.ContentBlock{Type: "resource"}},
		{name: "unknown block", block: core.ContentBlock{Type: "future_block"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAPI(t)
			err := a.client().reply("7")(context.Background(), []core.ContentBlock{tc.block})
			if err == nil || !strings.Contains(err.Error(), "deliver") {
				t.Fatalf("reply error = %v, want a delivery error", err)
			}
			if strings.Contains(err.Error(), "api-token") || strings.Contains(err.Error(), "private") {
				t.Fatalf("error leaks credentials or URI: %v", err)
			}
			a.silent(t)
		})
	}
}

func TestReplySplitsClassifiedTextURLsInOrder(t *testing.T) {
	a := newAPI(t)
	text := "See https://files.example/PHOTO.PNG, then (https://files.example/movie.mp4?x=1#view)! unknown https://files.example/item.unknownext. file https://files.example/report.PDF; done"
	if err := a.client().reply("7")(context.Background(), core.Text(text)); err != nil {
		t.Fatalf("reply: %v", err)
	}
	want := []string{
		`{"message":{"text":"See ","type":"text"}}`,
		`{"message":{"attachment":{"type":"image","url":"https://files.example/PHOTO.PNG"},"type":"attachment"}}`,
		`{"message":{"text":", then (","type":"text"}}`,
		`{"message":{"attachment":{"type":"video","url":"https://files.example/movie.mp4?x=1#view"},"type":"attachment"}}`,
		`{"message":{"text":")! unknown https://files.example/item.unknownext. file ","type":"text"}}`,
		`{"message":{"attachment":{"type":"file","url":"https://files.example/report.PDF"},"type":"attachment"}}`,
		`{"message":{"text":"; done","type":"text"}}`,
	}
	for i, body := range want {
		if got := a.next(t).body; got != body {
			t.Fatalf("call %d body = %s, want %s", i+1, got, body)
		}
	}
	a.silent(t)
}

func TestReplyPreservesBlockOrder(t *testing.T) {
	a := newAPI(t)
	content := []core.ContentBlock{
		{Type: "text", Text: "first"},
		{Type: "resource_link", URI: "https://files.example/report.pdf", MimeType: "application/pdf"},
		{Type: "text", Text: "last"},
	}
	if err := a.client().reply("7")(context.Background(), content); err != nil {
		t.Fatalf("reply: %v", err)
	}
	want := []string{
		`{"message":{"text":"first","type":"text"}}`,
		`{"message":{"attachment":{"type":"file","url":"https://files.example/report.pdf"},"type":"attachment"}}`,
		`{"message":{"text":"last","type":"text"}}`,
	}
	for i, body := range want {
		if got := a.next(t).body; got != body {
			t.Fatalf("call %d body = %s, want %s", i+1, got, body)
		}
	}
	a.silent(t)
}

func TestReplyStopsAfterAnInvalidBlock(t *testing.T) {
	a := newAPI(t)
	content := []core.ContentBlock{
		{Type: "text", Text: "sent"},
		{Type: "image", Data: "AAAA", MimeType: "image/png"},
		{Type: "text", Text: "not sent"},
	}
	if err := a.client().reply("7")(context.Background(), content); err == nil {
		t.Fatal("reply succeeded")
	}
	if got := a.next(t).body; got != `{"message":{"text":"sent","type":"text"}}` {
		t.Fatalf("first body = %s", got)
	}
	a.silent(t)
}

func TestReplyStopsAfterAnAPIFailure(t *testing.T) {
	requests := make(chan apiRequest, 3)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- apiRequest{body: string(body)}
		calls++
		if calls == 2 {
			http.Error(w, "failed", http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	c := &client{url: srv.URL, token: "secret-token", http: srv.Client()}
	content := []core.ContentBlock{
		{Type: "text", Text: "sent"},
		{Type: "resource_link", URI: "https://files.example/report.pdf", MimeType: "application/pdf"},
		{Type: "text", Text: "not sent"},
	}

	err := c.reply("7")(context.Background(), content)
	if err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("reply error = %v", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error leaks token: %v", err)
	}
	if got := len(requests); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
	if got := (<-requests).body; got != `{"message":{"text":"sent","type":"text"}}` {
		t.Fatalf("first body = %s", got)
	}
	if got := (<-requests).body; got != `{"message":{"attachment":{"type":"file","url":"https://files.example/report.pdf"},"type":"attachment"}}` {
		t.Fatalf("second body = %s", got)
	}
}

func TestReplyReportsARefusal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusBadGateway} {
		a := newAPI(t)
		a.status = status
		a.body = "no"
		err := a.client().reply("12345")(context.Background(), core.Text("hello"))
		if err == nil {
			t.Fatalf("status %d: reply succeeded, want an error", status)
		}
		if !strings.Contains(err.Error(), "status "+strconv.Itoa(status)) {
			t.Fatalf("error %q does not name status %d", err, status)
		}
		if !strings.Contains(err.Error(), "no") {
			t.Fatalf("error %q does not carry the response body", err)
		}
	}
}

func TestEchoAnswersAnIncomingMessageOnlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		event   string
		answers bool
	}{
		{name: "incoming message is answered", event: eventReceived, answers: true},
		// Echoing an outgoing message would answer momo's own reply.
		{name: "outgoing message is not", event: eventSent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAPI(t)
			h := &webhook{
				secret: secret,
				core:   echoHandler(),
				client: a.client(),
			}
			body := payload(tc.event)
			post(t, h, body, sign(body, secret))
			if !tc.answers {
				a.silent(t)
				return
			}
			got := a.next(t)
			if got.path != "/contact/id:12345/message" || got.body != `{"message":{"text":"hello","type":"text"}}` {
				t.Fatalf("API call = %+v, want the same text back to contact 12345", got)
			}
			a.silent(t)
		})
	}
}

func TestConcurrentWebhooksEachReachTheirOwnContact(t *testing.T) {
	a := newAPI(t)
	log := echoHandler()
	for _, id := range []string{"111", "222"} {
		body := `{"event_type":"message.received","contact":{"id":` + id + `},` +
			`"message":{"message":{"type":"text","text":"hi"}}}`
		go post(t, &webhook{secret: secret, core: log, client: a.client()}, body, sign(body, secret))
	}
	paths := map[string]bool{a.next(t).path: true}
	paths[a.next(t).path] = true
	if !paths["/contact/id:111/message"] || !paths["/contact/id:222/message"] {
		t.Fatalf("paths called = %v, want one call per contact", paths)
	}
	a.silent(t)
}

func TestAFailedTurnLeavesACommentAndNoMessage(t *testing.T) {
	a := newAPI(t)
	h := &webhook{secret: secret, core: failingHandler(), client: a.client()}
	body := payload(eventReceived)
	post(t, h, body, sign(body, secret))

	got := a.next(t)
	if got.path != "/contact/id:12345/comment" {
		t.Fatalf("API call = %+v, want an internal comment on contact 12345", got)
	}
	if !strings.Contains(got.body, "the agent exited before it replied") {
		t.Fatalf("comment body = %s, does not carry the reason the turn failed", got.body)
	}
	a.silent(t)
}

func TestNewDefaultsTheAPIURL(t *testing.T) {
	decode := func(v any) error {
		s := v.(*settings)
		s.ReceivedSecret = "a"
		s.SentSecret = "b"
		s.APIToken = "api-token"
		return nil
	}
	c, err := New(context.Background(), decode, capture{}, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, ok := c.Routes()[0].Handler.(*webhook)
	if !ok {
		t.Fatalf("handler is %T, want *webhook", c.Routes()[0].Handler)
	}
	if h.client.url != "https://api.respond.io/v2" {
		t.Fatalf("api_url = %q, want respond.io's v2 API", h.client.url)
	}
}
