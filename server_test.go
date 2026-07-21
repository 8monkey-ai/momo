package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func incomingText(contactID int64, text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"event_type": "message.received",
		"contact":    map[string]any{"id": contactID},
		"message": map[string]any{
			"messageId": 42,
			"message":   map[string]any{"type": "text", "text": text},
		},
	})
	return b
}

func TestWebhookAcceptsKnownEvents(t *testing.T) {
	ts := httptest.NewServer(newServer(config{}).routes())
	defer ts.Close()

	for _, eventType := range []string{"message.received", "message.sent", "contact.updated"} {
		body, _ := json.Marshal(map[string]any{"event_type": eventType})
		resp, err := http.Post(ts.URL+"/webhook", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: got %d, want 200", eventType, resp.StatusCode)
		}
	}
}

func TestWebhookRejectsBadPayload(t *testing.T) {
	ts := httptest.NewServer(newServer(config{}).routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/webhook", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d, want 400", resp.StatusCode)
	}
}

func TestSignatureVerification(t *testing.T) {
	body := incomingText(1, "hi")
	key := "test-signing-key"
	ts := httptest.NewServer(newServer(config{incomingSigningKey: key}).routes())
	defer ts.Close()

	post := func(sig string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook", strings.NewReader(string(body)))
		if sig != "" {
			req.Header.Set("X-Webhook-Signature", sig)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(""); code != http.StatusUnauthorized {
		t.Errorf("missing signature: got %d, want 401", code)
	}
	if code := post("bm90LXRoZS1zaWduYXR1cmU="); code != http.StatusUnauthorized {
		t.Errorf("wrong signature: got %d, want 401", code)
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	valid := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if code := post(valid); code != http.StatusOK {
		t.Errorf("valid signature: got %d, want 200", code)
	}
}

func TestHealthz(t *testing.T) {
	ts := httptest.NewServer(newServer(config{}).routes())
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
