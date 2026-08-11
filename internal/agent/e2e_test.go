package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/channel/respondio"
	"github.com/8monkey-ai/momo/internal/core"
)

// TestAMessageOnAChannelIsAnsweredByTheAgent walks the whole path: a respond.io
// webhook arrives, the harness runs the turn, and the reply leaves through
// respond.io's API.
func TestAMessageOnAChannelIsAnsweredByTheAgent(t *testing.T) {
	sent := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		sent <- r.URL.Path + " " + payload.Message.Text
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	h := newHarness(t)
	c, err := respondio.New(context.Background(), func(v any) error {
		return yaml.Unmarshal([]byte("received_secret: s\nsent_secret: s\napi_token: t\napi_url: "+api.URL+"\n"), v)
	}, core.Qualify("respondio", &core.AgentHandler{Agent: h, Log: discard()}))
	if err != nil {
		t.Fatalf("respondio.New: %v", err)
	}

	body := `{"event_type":"message.received","contact":{"id":12345},` +
		`"message":{"message":{"type":"text","text":"hello"}}}`
	mac := hmac.New(sha256.New, []byte("s"))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, received(t, c).Path, strings.NewReader(body))
	req.Header.Set("X-Webhook-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	received(t, c).Handler.ServeHTTP(w, req)

	got := <-sent
	if !strings.HasPrefix(got, "/contact/id:12345/message ") || !strings.HasSuffix(got, "echo:hello") {
		t.Fatalf("respond.io was sent %q, want the agent's reply to contact 12345", got)
	}
}

func received(t *testing.T, c channel.Channel) channel.Route {
	t.Helper()
	for _, route := range c.Routes() {
		if strings.HasSuffix(route.Path, "/received") {
			return route
		}
	}
	t.Fatal("the channel serves no route for received messages")
	return channel.Route{}
}
