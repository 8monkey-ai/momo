package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/channel"
)

type fakeChannel struct{}

func (fakeChannel) Start(_ channel.Handler, mux *http.ServeMux) {
	mux.HandleFunc("POST /fake", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestChannelRegistersItsOwnRoutes(t *testing.T) {
	srv := &server{channels: []channel.Channel{fakeChannel{}}}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/fake", "application/json", nil)
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

// fakeRespond records sends as "contactID: text" so tests pin the recipient too.
type fakeRespond struct{ messages chan string }

func (f *fakeRespond) handler() http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body struct {
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		contactID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/contact/id:"), "/message")
		f.messages <- contactID + ": " + body.Message.Text
	})
}

func (f *fakeRespond) waitForMessage(t *testing.T) string {
	t.Helper()
	select {
	case msg := <-f.messages:
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a message")
		return ""
	}
}

func TestEchoRoundTrip(t *testing.T) {
	fake := &fakeRespond{messages: make(chan string, 1)}
	respondSrv := httptest.NewServer(fake.handler())
	defer respondSrv.Close()

	ch, err := channel.New("respondio", map[string]string{
		"api_token": "test-token",
		"api_url":   respondSrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &server{channels: []channel.Channel{ch}}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	incoming := `{"event_type":"message.received","contact":{"id":7},"message":{"message":{"type":"text","text":"hello"}}}`
	resp, err := http.Post(ts.URL+"/respondio", "application/json",
		strings.NewReader(incoming))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook status %d", resp.StatusCode)
	}

	msg := fake.waitForMessage(t)
	if msg != "7: You said: hello" {
		t.Errorf("echoed message = %q, want %q", msg, "7: You said: hello")
	}
}
