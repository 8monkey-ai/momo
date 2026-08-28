package respondio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

type savedAttachment struct {
	conversation string
	name         string
	body         string
}

type attachmentStore struct {
	mu      sync.Mutex
	names   []string
	saves   []savedAttachment
	failed  int
	saveErr error
}

func (s *attachmentStore) Save(_ context.Context, conversation, name string, r io.Reader) (string, string, error) {
	body, err := io.ReadAll(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failed++
		return "", "", err
	}
	if s.saveErr != nil {
		s.failed++
		return "", "", s.saveErr
	}
	s.saves = append(s.saves, savedAttachment{conversation: conversation, name: name, body: string(body)})
	safeName := name
	if len(s.names) >= len(s.saves) {
		safeName = s.names[len(s.saves)-1]
	}
	return safeName, "file:///saved/" + safeName, nil
}

func attachmentPayload(kind, rawAttachment string, assigneeID int64) string {
	assignee := "null"
	if assigneeID != 0 {
		assignee = `{"id":` + strconv.FormatInt(assigneeID, 10) + `}`
	}
	return `{"event_type":"message.received","contact":{"id":12345,"assignee":` + assignee + `},` +
		`"message":{"message":{"type":"attachment","attachment":{"type":"` + kind + `",` + rawAttachment + `}}}}`
}

func decodedEvent(t *testing.T, body string) event {
	t.Helper()
	var ev event
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func attachmentWebhook(c capture, store core.ConversationFiles, httpClient *http.Client, max int64) *webhook {
	return &webhook{
		core:               c,
		client:             &client{http: httpClient},
		files:              store,
		maxAttachmentBytes: max,
	}
}

func nextCall(t *testing.T, c capture) call {
	t.Helper()
	select {
	case got := <-c.calls:
		return got
	case <-time.After(time.Second):
		t.Fatal("core was never called")
		return call{}
	}
}

func TestInboundAttachmentKindsBecomeResourceLinks(t *testing.T) {
	for _, kind := range []string{"image", "video", "audio", "file"} {
		t.Run(kind, func(t *testing.T) {
			download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, kind)
			}))
			defer download.Close()
			store := &attachmentStore{names: []string{"safe-" + kind + ".bin"}}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, download.Client(), 100)
			body := attachmentPayload(kind, `"url":"`+download.URL+`/remote.bin","fileName":"provider.bin","mimeType":"application/x-provider"`, 0)

			h.dispatch(context.Background(), decodedEvent(t, body))

			got := nextCall(t, c)
			want := core.Message{Conversation: "12345", Content: []core.ContentBlock{{
				Type: "resource_link", URI: "file:///saved/safe-" + kind + ".bin", Name: "safe-" + kind + ".bin",
				MimeType: "application/x-provider", Size: int64(len(kind)),
			}}}
			if !reflect.DeepEqual(got.message, want) {
				t.Fatalf("message = %+v, want %+v", got.message, want)
			}
			if len(store.saves) != 1 || store.saves[0] != (savedAttachment{conversation: "12345", name: "provider.bin", body: kind}) {
				t.Fatalf("saves = %+v", store.saves)
			}
		})
	}
}

func TestInboundAttachmentMetadataPriority(t *testing.T) {
	for _, tc := range []struct {
		name             string
		provider         string
		headers          map[string]string
		path             string
		wantProposedName string
		wantMIME         string
	}{
		{name: "provider", provider: `,"name":"provider.jpg","mimeType":"image/provider"`, headers: map[string]string{"Content-Disposition": `attachment; filename="header.mp4"`, "Content-Type": "video/header"}, path: "/url.mp3", wantProposedName: "provider.jpg", wantMIME: "image/provider"},
		{name: "headers", headers: map[string]string{"Content-Disposition": `attachment; filename="header.mp4"`, "Content-Type": "video/header"}, path: "/url.mp3", wantProposedName: "header.mp4", wantMIME: "video/header"},
		{name: "final URL", headers: map[string]string{"Content-Type": ""}, path: "/media/Sound.MP3", wantProposedName: "Sound.MP3", wantMIME: "audio/mpeg"},
		{name: "fallback", headers: map[string]string{"Content-Type": ""}, path: "/", wantProposedName: "attachment", wantMIME: "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for name, value := range tc.headers {
					w.Header().Set(name, value)
				}
				_, _ = io.WriteString(w, "data")
			}))
			defer download.Close()
			store := &attachmentStore{}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, download.Client(), 100)
			body := attachmentPayload("file", `"url":"`+download.URL+tc.path+`"`+tc.provider, 0)

			h.dispatch(context.Background(), decodedEvent(t, body))

			got := nextCall(t, c).message.Content[0]
			if len(store.saves) != 1 || store.saves[0].name != tc.wantProposedName {
				t.Fatalf("saved name = %+v, want %q", store.saves, tc.wantProposedName)
			}
			if got.MimeType != tc.wantMIME {
				t.Fatalf("MIME = %q, want %q", got.MimeType, tc.wantMIME)
			}
		})
	}
}

func TestInboundAttachmentUsesSafeUniqueStoreNames(t *testing.T) {
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "x") }))
	defer download.Close()
	store := &attachmentStore{names: []string{"photo.jpg", "photo-2.jpg"}}
	c := capture{calls: make(chan call, 2)}
	h := attachmentWebhook(c, store, download.Client(), 100)
	body := attachmentPayload("image", `"url":"`+download.URL+`/x","name":"../photo.jpg"`, 0)

	h.dispatch(context.Background(), decodedEvent(t, body))
	h.dispatch(context.Background(), decodedEvent(t, body))

	for i, want := range []string{"photo.jpg", "photo-2.jpg"} {
		block := nextCall(t, c).message.Content[0]
		if block.Name != want || block.URI != "file:///saved/"+want {
			t.Fatalf("turn %d block = %+v, want saved name %q", i, block, want)
		}
	}
}

func TestInboundAttachmentURLAndRedirectValidation(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer good.Close()
	redirectGood := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, good.URL+"/final.txt", http.StatusFound)
	}))
	defer redirectGood.Close()
	redirectBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///private/secret", http.StatusFound)
	}))
	defer redirectBad.Close()

	for _, tc := range []struct {
		name   string
		url    string
		saved  bool
		reason string
	}{
		{name: "successful redirect", url: redirectGood.URL, saved: true},
		{name: "unsupported initial scheme", url: "ftp://example.com/file.txt", reason: "invalid address"},
		{name: "relative address", url: "/file.txt", reason: "invalid address"},
		{name: "address without host", url: "https:///file.txt", reason: "invalid address"},
		{name: "unsupported redirect scheme", url: redirectBad.URL, reason: "invalid redirect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &attachmentStore{}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, good.Client(), 100)
			body := attachmentPayload("file", `"url":"`+tc.url+`","name":"file.txt"`, 0)

			h.dispatch(context.Background(), decodedEvent(t, body))

			block := nextCall(t, c).message.Content[0]
			if tc.saved {
				if len(store.saves) != 1 || store.saves[0].body != "ok" || block.Type != "resource_link" {
					t.Fatalf("save = %+v, block = %+v", store.saves, block)
				}
				return
			}
			if len(store.saves) != 0 || block.Type != "text" || !strings.Contains(block.Text, tc.reason) {
				t.Fatalf("save = %+v, block = %+v, want %q", store.saves, block, tc.reason)
			}
			if strings.Contains(block.Text, "private") || strings.Contains(block.Text, "example.com") {
				t.Fatalf("block leaks URL: %+v", block)
			}
		})
	}
}

func TestInboundAttachmentLimitsReachTheStoreAsReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   string
		body     string
		redirect bool
		failed   int
		saved    int
		reason   string
	}{
		{name: "declared", header: "4", body: "data", reason: "too large"},
		{name: "streamed", body: "data", failed: 1, reason: "too large"},
		{name: "redirect declared", header: "4", body: "data", redirect: true, reason: "too large"},
		{name: "redirect streamed", body: "data", redirect: true, failed: 1, reason: "too large"},
		{name: "exact limit", header: "3", body: "abc", saved: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.redirect && r.URL.Path != "/final" {
					http.Redirect(w, r, "/final", http.StatusFound)
					return
				}
				if tc.header != "" {
					w.Header().Set("Content-Length", tc.header)
				} else {
					w.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer download.Close()
			store := &attachmentStore{}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, download.Client(), 3)
			body := attachmentPayload("file", `"url":"`+download.URL+`/file.bin","name":"file.bin"`, 0)

			h.dispatch(context.Background(), decodedEvent(t, body))

			block := nextCall(t, c).message.Content[0]
			if len(store.saves) != tc.saved || store.failed != tc.failed {
				t.Fatalf("saved = %d, failed reads = %d, want %d and %d", len(store.saves), store.failed, tc.saved, tc.failed)
			}
			if tc.reason != "" && (block.Type != "text" || !strings.Contains(block.Text, tc.reason)) {
				t.Fatalf("block = %+v, want unavailable reason %q", block, tc.reason)
			}
			if tc.saved == 1 && block.Size != 3 {
				t.Fatalf("size = %d, want 3", block.Size)
			}
		})
	}
}

func TestInboundAttachmentFailuresStillRunTheTurnWithoutLeakingURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		http.Error(w, "secret response", http.StatusBadGateway)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name      string
		url       string
		storeErr  error
		wantName  string
		wantCause string
	}{
		{name: "unsupported scheme", url: "file:///private/token", wantName: "badname.txt", wantCause: "invalid address"},
		{name: "HTTP failure", url: server.URL + "/token?secret=yes", wantName: "badname.txt", wantCause: "download failed"},
		{name: "storage failure", url: server.URL + "/ok", storeErr: errors.New("disk /private/token failed"), wantName: "badname.txt", wantCause: "storage failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &attachmentStore{saveErr: tc.storeErr}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, server.Client(), 100)
			body := attachmentPayload("file", `"url":"`+tc.url+`","name":"../bad\nname.txt"`, 0)

			h.dispatch(context.Background(), decodedEvent(t, body))

			block := nextCall(t, c).message.Content[0]
			if block.Type != "text" || !strings.Contains(block.Text, tc.wantName) || !strings.Contains(block.Text, tc.wantCause) {
				t.Fatalf("block = %+v, want unavailable name and reason", block)
			}
			for _, secret := range []string{"token", "secret=yes", "/private"} {
				if strings.Contains(block.Text, secret) {
					t.Fatalf("unavailable text %q leaks %q", block.Text, secret)
				}
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	read   func([]byte) (int, error)
	closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) { return b.read(p) }
func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

func TestInboundAttachmentReadAndCancellationFailuresCleanTheSaveAndCloseTheBody(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       func(context.Context) *trackedBody
		wantReason string
	}{
		{name: "read failure", wantReason: "download failed", body: func(context.Context) *trackedBody {
			firstRead := true
			return &trackedBody{read: func(p []byte) (int, error) {
				if firstRead {
					firstRead = false
					return copy(p, "partial"), nil
				}
				return 0, errors.New("remote read failed")
			}}
		}},
		{name: "cancelled", wantReason: "download cancelled", body: func(ctx context.Context) *trackedBody {
			return &trackedBody{read: func([]byte) (int, error) {
				<-ctx.Done()
				return 0, ctx.Err()
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var body *trackedBody
			started := make(chan struct{})
			httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body = tc.body(r.Context())
				if tc.name == "cancelled" {
					read := body.read
					body.read = func(p []byte) (int, error) {
						close(started)
						return read(p)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header), Request: r, ContentLength: -1}, nil
			})}
			store := &attachmentStore{}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, httpClient, 100)
			ev := decodedEvent(t, attachmentPayload("file", `"url":"https://example.test/file.bin"`, 0))
			done := make(chan struct{})
			go func() {
				h.dispatch(ctx, ev)
				close(done)
			}()
			if tc.name == "cancelled" {
				<-started
				cancel()
			}
			<-done

			block := nextCall(t, c).message.Content[0]
			if store.failed != 1 || len(store.saves) != 0 {
				t.Fatalf("failed saves = %d, completed = %+v", store.failed, store.saves)
			}
			if block.Type != "text" || !strings.Contains(block.Text, tc.wantReason) {
				t.Fatalf("block = %+v, want %q", block, tc.wantReason)
			}
			if body == nil || !body.closed {
				t.Fatal("response body was not closed")
			}
		})
	}
}

func TestOnlyOwnedReceivedAttachmentsRunTurns(t *testing.T) {
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "x") }))
	defer download.Close()
	for _, tc := range []struct {
		name      string
		eventType string
		assignee  int64
		called    bool
	}{
		{name: "owned received", eventType: eventReceived, called: true},
		{name: "another assignee", eventType: eventReceived, assignee: 77},
		{name: "sent", eventType: eventSent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &attachmentStore{}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, download.Client(), 100)
			h.momoAssigneeID = 42
			body := attachmentPayload("file", `"url":"`+download.URL+`/x"`, tc.assignee)
			ev := decodedEvent(t, body)
			ev.EventType = tc.eventType

			h.dispatch(context.Background(), ev)

			select {
			case <-c.calls:
				if !tc.called {
					t.Fatal("core was called")
				}
			case <-time.After(50 * time.Millisecond):
				if tc.called {
					t.Fatal("core was not called")
				}
			}
		})
	}
}

func TestWebhookAcknowledgesBeforeAttachmentDownload(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "ok")
	}))
	defer download.Close()
	store := &attachmentStore{}
	c := capture{calls: make(chan call, 1)}
	h := attachmentWebhook(c, store, download.Client(), 100)
	h.secret = secret
	body := attachmentPayload("file", `"url":"`+download.URL+`/x"`, 0)

	response := post(t, h, body, sign(body, secret))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	select {
	case got := <-c.calls:
		t.Fatalf("turn ran before download finished: %+v", got)
	default:
	}
	close(release)
	nextCall(t, c)
}

func TestMaxAttachmentBytesSetting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   *int64
		valid bool
		want  int64
	}{
		{name: "default", valid: true, want: 20_000_000},
		{name: "configured", set: ptr(int64(123)), valid: true, want: 123},
		{name: "zero", set: ptr(int64(0))},
		{name: "negative", set: ptr(int64(-1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decode := func(v any) error {
				s := v.(*settings)
				s.ReceivedSecret, s.SentSecret, s.APIToken = "a", "b", "token"
				if tc.set != nil {
					s.MaxAttachmentBytes = *tc.set
				}
				return nil
			}
			built, err := New(context.Background(), decode, capture{}, nil, &attachmentStore{})
			if !tc.valid {
				if err == nil {
					t.Fatal("New succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			h := built.Routes()[0].Handler.(*webhook)
			if h.maxAttachmentBytes != tc.want {
				t.Fatalf("max = %d, want %d", h.maxAttachmentBytes, tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
