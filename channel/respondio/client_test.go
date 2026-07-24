package respondio

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendTextRequest(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer ts.Close()

	ch := respondio{"api_token": "tok", "api_url": ts.URL}
	if err := ch.SendText("7", "hello"); err != nil {
		t.Fatal(err)
	}

	if got.Method != http.MethodPost || got.URL.Path != "/contact/id:7/message" {
		t.Errorf("got %s %s, want POST /contact/id:7/message", got.Method, got.URL.Path)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer tok")
	}
	want := `{"message":{"text":"hello","type":"text"}}`
	if string(gotBody) != want {
		t.Errorf("body = %s, want %s", gotBody, want)
	}
}

func TestSendTextAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such contact", http.StatusNotFound)
	}))
	defer ts.Close()

	err := respondio{"api_url": ts.URL}.SendText("7", "hello")
	if err == nil || !strings.Contains(err.Error(), "no such contact") {
		t.Errorf("err = %v, want API error mentioning response body", err)
	}
}
