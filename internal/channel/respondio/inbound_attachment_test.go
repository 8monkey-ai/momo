package respondio

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
		core:                c,
		client:              &client{http: httpClient},
		files:               store,
		maxAttachmentBytes:  max,
		allowAttachmentAddr: func(netip.Addr) bool { return true },
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

func TestPublicAttachmentAddressPolicy(t *testing.T) {
	for _, tc := range []struct {
		address string
		public  bool
	}{
		{address: "127.0.0.1"},
		{address: "::1"},
		{address: "10.0.0.1"},
		{address: "172.16.0.1"},
		{address: "192.168.0.1"},
		{address: "fc00::1"},
		{address: "fe80::1"},
		{address: "169.254.1.1"},
		{address: "224.0.0.1"},
		{address: "ff02::1"},
		{address: "0.0.0.0"},
		{address: "::"},
		{address: "100.64.0.1"},
		{address: "169.254.169.254"},
		{address: "::ffff:127.0.0.1"},
		{address: "::ffff:169.254.169.254"},
		{address: "192.0.0.1"},
		{address: "64:ff9b::1"},
		{address: "192.0.2.1"},
		{address: "2001:db8::1"},
		{address: "3fff::1"},
		{address: "198.18.0.1"},
		{address: "2001:2::1"},
		{address: "240.0.0.1"},
		{address: "100::1"},
		{address: "8.8.8.8", public: true},
		{address: "::ffff:8.8.8.8", public: true},
		{address: "2606:4700:4700::1111", public: true},
	} {
		t.Run(tc.address, func(t *testing.T) {
			if got := publicAttachmentAddr(netip.MustParseAddr(tc.address)); got != tc.public {
				t.Fatalf("publicAttachmentAddr(%s) = %t, want %t", tc.address, got, tc.public)
			}
		})
	}
}

func TestDefaultWebhookCannotDownloadFromLoopback(t *testing.T) {
	requested := make(chan struct{}, 1)
	download := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requested <- struct{}{} }))
	defer download.Close()
	store := &attachmentStore{}
	c := capture{calls: make(chan call, 1)}
	h := &webhook{core: c, client: &client{http: download.Client()}, files: store, maxAttachmentBytes: 100}

	h.dispatch(context.Background(), decodedEvent(t, attachmentPayload("file", `"url":"`+download.URL+`/file.txt"`, 0)))

	if block := nextCall(t, c).message.Content[0]; block.Type != "text" || !strings.Contains(block.Text, "download failed") {
		t.Fatalf("block = %+v, want failed download", block)
	}
	select {
	case <-requested:
		t.Fatal("loopback server received a request")
	default:
	}
	if len(store.saves) != 0 {
		t.Fatalf("saves = %+v, want none", store.saves)
	}
}

func TestDefaultWebhookCannotDownloadFromMappedLoopbackLiteral(t *testing.T) {
	requested := make(chan struct{}, 1)
	download := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requested <- struct{}{} }))
	defer download.Close()
	_, port, err := net.SplitHostPort(download.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	store := &attachmentStore{}
	c := capture{calls: make(chan call, 1)}
	h := &webhook{core: c, client: &client{http: download.Client()}, files: store, maxAttachmentBytes: 100}
	mappedURL := "http://[::ffff:127.0.0.1]:" + port + "/file.txt"

	h.dispatch(context.Background(), decodedEvent(t, attachmentPayload("file", `"url":"`+mappedURL+`"`, 0)))

	if block := nextCall(t, c).message.Content[0]; block.Type != "text" || !strings.Contains(block.Text, "download failed") {
		t.Fatalf("block = %+v, want failed download", block)
	}
	select {
	case <-requested:
		t.Fatal("loopback server received a request")
	default:
	}
	if len(store.saves) != 0 {
		t.Fatalf("saves = %+v, want none", store.saves)
	}
}

func TestAttachmentAddressTestSeamCanAllowLoopback(t *testing.T) {
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer download.Close()
	store := &attachmentStore{}
	c := capture{calls: make(chan call, 1)}
	h := attachmentWebhook(c, store, download.Client(), 100)

	h.dispatch(context.Background(), decodedEvent(t, attachmentPayload("file", `"url":"`+download.URL+`/file.txt"`, 0)))

	if block := nextCall(t, c).message.Content[0]; block.Type != "resource_link" {
		t.Fatalf("block = %+v, want resource link", block)
	}
}

func TestAttachmentRedirectAppliesAddressPolicyAgain(t *testing.T) {
	requested := make(chan struct{}, 1)
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested <- struct{}{}
		_, _ = io.WriteString(w, "secret")
	}))
	defer final.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/final.txt", http.StatusFound)
	}))
	defer redirect.Close()
	store := &attachmentStore{}
	c := capture{calls: make(chan call, 1)}
	h := attachmentWebhook(c, store, redirect.Client(), 100)
	checks := 0
	h.allowAttachmentAddr = func(netip.Addr) bool {
		checks++
		return checks == 1
	}

	h.dispatch(context.Background(), decodedEvent(t, attachmentPayload("file", `"url":"`+redirect.URL+`/start"`, 0)))

	if block := nextCall(t, c).message.Content[0]; block.Type != "text" || !strings.Contains(block.Text, "download failed") {
		t.Fatalf("block = %+v, want failed redirect download", block)
	}
	if checks != 2 {
		t.Fatalf("address policy checks = %d, want 2", checks)
	}
	select {
	case <-requested:
		t.Fatal("redirect destination received a request")
	default:
	}
	if len(store.saves) != 0 {
		t.Fatalf("saves = %+v, want none", store.saves)
	}
}

func TestAttachmentDownloadClientIsCachedPerWebhook(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	source := &http.Client{Transport: transport, Timeout: 7 * time.Second}
	h := attachmentWebhook(capture{}, nil, source, 100)
	other := attachmentWebhook(capture{}, nil, source, 100)

	first := h.downloadClient()
	second := h.downloadClient()
	otherClient := other.downloadClient()

	if first != second {
		t.Fatal("download client was rebuilt")
	}
	if first.Transport != second.Transport {
		t.Fatal("download transport was rebuilt")
	}
	if first == otherClient || first.Transport == otherClient.Transport {
		t.Fatal("webhooks share a download client or transport")
	}
	configured := first.Transport.(*http.Transport)
	if configured == transport || configured.Proxy != nil || transport.Proxy == nil {
		t.Fatal("download transport did not clone and disable the proxy")
	}
	if configured.TLSClientConfig == transport.TLSClientConfig || configured.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("download transport did not preserve cloned TLS settings")
	}
	if first.Timeout != 7*time.Second {
		t.Fatalf("timeout = %v, want 7s", first.Timeout)
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

func TestInboundAttachmentReadAndCancellationFailuresCleanTheSave(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantReason string
		wantFailed int
	}{
		{name: "read failure", wantReason: "download failed", wantFailed: 1},
		{name: "cancelled", wantReason: "download cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "read failure" {
					connection, buffer, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\npartial")
					_ = buffer.Flush()
					_ = connection.Close()
					return
				}
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			}))
			defer download.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &attachmentStore{}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, download.Client(), 100)
			ev := decodedEvent(t, attachmentPayload("file", `"url":"`+download.URL+`/file.bin"`, 0))
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
			if store.failed != tc.wantFailed || len(store.saves) != 0 {
				t.Fatalf("failed saves = %d, completed = %+v", store.failed, store.saves)
			}
			if block.Type != "text" || !strings.Contains(block.Text, tc.wantReason) {
				t.Fatalf("block = %+v, want %q", block, tc.wantReason)
			}
		})
	}
}

func TestUnownedReceivedAttachmentRecordsSafeMarkerWithoutDownload(t *testing.T) {
	requested := make(chan struct{}, 1)
	download := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requested <- struct{}{} }))
	defer download.Close()
	store := &attachmentStore{}
	c := capture{calls: make(chan call, 1)}
	h := attachmentWebhook(c, store, download.Client(), 100)
	h.history = c
	h.momoAssigneeID = 42
	body := attachmentPayload("file", `"url":"`+download.URL+`/private/path/fallback.txt?token=secret","fileName":"../report.pdf"`, 77)

	h.dispatch(context.Background(), decodedEvent(t, body))

	got := nextCall(t, c)
	want := call{direction: "recorded user", message: core.Message{Conversation: "12345", Content: core.Text(`Attachment "report.pdf" received.`)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("call = %+v, want %+v", got, want)
	}
	select {
	case <-requested:
		t.Fatal("attachment was downloaded")
	default:
	}
	if len(store.saves) != 0 {
		t.Fatalf("saves = %+v, want none", store.saves)
	}
}

func TestSentOperatorAndWorkflowAttachmentsRemainIgnored(t *testing.T) {
	for _, source := range []string{senderUser, senderWorkflow} {
		t.Run(source, func(t *testing.T) {
			store := &attachmentStore{}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, http.DefaultClient, 100)
			ev := decodedEvent(t, attachmentPayload("file", `"url":"http://127.0.0.1/private"`, 0))
			ev.EventType = eventSent
			ev.Sender.Source = source

			h.dispatch(context.Background(), ev)

			select {
			case got := <-c.calls:
				t.Fatalf("core was called with %+v", got)
			case <-time.After(50 * time.Millisecond):
			}
			if len(store.saves) != 0 {
				t.Fatalf("saves = %+v, want none", store.saves)
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
		called    bool
	}{
		{name: "owned received", eventType: eventReceived, called: true},
		{name: "sent", eventType: eventSent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &attachmentStore{}
			c := capture{calls: make(chan call, 1)}
			h := attachmentWebhook(c, store, download.Client(), 100)
			h.momoAssigneeID = 42
			body := attachmentPayload("file", `"url":"`+download.URL+`/x"`, 0)
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
