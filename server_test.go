package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/8monkey-ai/agent-server/channel/respondio"
)

func TestWebhookRoutedWhenConfigured(t *testing.T) {
	cfg := config{respondio: respondio.Config{OutgoingSigningKey: "k"}}
	ts := httptest.NewServer((&server{cfg: cfg}).routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/webhook/respondio", "application/json", strings.NewReader(`{"event_type":"message.received"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got %d, want 200", resp.StatusCode)
	}
}

func TestWebhookNotRoutedWhenUnconfigured(t *testing.T) {
	ts := httptest.NewServer((&server{}).routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/webhook/respondio", "application/json", strings.NewReader(`{"event_type":"message.received"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("got %d, want 404", resp.StatusCode)
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
