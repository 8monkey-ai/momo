package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
