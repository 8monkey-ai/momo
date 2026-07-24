package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/channel"
)

type fakeWebhookChannel struct{}

func (fakeWebhookChannel) SendText(string, string) error { return nil }

func (fakeWebhookChannel) Webhook(channel.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestWebhookRoutedPerChannel(t *testing.T) {
	srv := &server{channels: map[string]channel.Channel{"fake": fakeWebhookChannel{}}}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/webhook/fake", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got %d, want 200", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	ts := httptest.NewServer((&server{}).routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got %d, want 200", resp.StatusCode)
	}
}

// fakeRespond captures messages the server sends back to respond.io,
// recorded as "contactID: text" so tests pin the recipient too.
type fakeRespond struct {
	mu       sync.Mutex
	messages []string
}

func (f *fakeRespond) handler() http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body struct {
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		contactID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/contact/id:"), "/message")
		f.mu.Lock()
		f.messages = append(f.messages, contactID+": "+body.Message.Text)
		f.mu.Unlock()
	})
}

func (f *fakeRespond) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.messages) >= n {
			out := slices.Clone(f.messages)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("timed out waiting for %d messages, got %q", n, f.messages)
	return nil
}

func incomingText(contactID int64, text string) string {
	b, _ := json.Marshal(map[string]any{
		"event_type": "message.received",
		"contact":    map[string]any{"id": contactID},
		"message": map[string]any{
			"messageId": 42,
			"message":   map[string]any{"type": "text", "text": text},
		},
	})
	return string(b)
}

func TestEchoRoundTrip(t *testing.T) {
	fake := &fakeRespond{}
	respondSrv := httptest.NewServer(fake.handler())
	defer respondSrv.Close()

	ch, err := channel.New("respondio", map[string]string{
		"api_token": "test-token",
		"api_url":   respondSrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &server{channels: map[string]channel.Channel{"respondio": ch}}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/webhook/respondio", "application/json",
		strings.NewReader(incomingText(7, "hello")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook status %d", resp.StatusCode)
	}

	msgs := fake.wait(t, 1)
	if msgs[0] != "7: You said: hello" {
		t.Errorf("echoed message = %q, want %q", msgs[0], "7: You said: hello")
	}
}
