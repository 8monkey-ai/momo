package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/channel"
	_ "github.com/8monkey-ai/momo/internal/channel/respondio"
	"github.com/8monkey-ai/momo/internal/core"
)

const webhookSecret = "signing-key"

// sentMessage is one call momo made to respond.io's send-a-message API.
type sentMessage struct {
	path string
	text string
}

// respondIoAPI stands in for respond.io's REST API and records what momo sent.
type respondIoAPI struct {
	url  string
	sent chan sentMessage
}

func newRespondIoAPI(t *testing.T) *respondIoAPI {
	t.Helper()
	api := &respondIoAPI{sent: make(chan sentMessage, 4)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		api.sent <- sentMessage{path: r.URL.Path, text: body.Message.Text}
	}))
	t.Cleanup(srv.Close)
	api.url = srv.URL
	return api
}

func (api *respondIoAPI) next(t *testing.T) sentMessage {
	t.Helper()
	select {
	case m := <-api.sent:
		return m
	case <-time.After(30 * time.Second):
		t.Fatal("momo sent nothing to respond.io")
		return sentMessage{}
	}
}

// respondIoWebhook builds the respond.io channel around the agent and returns the
// route incoming messages arrive on. The channel is built the way momo builds it,
// so the qualified conversation identity is the one a turn runs under.
func respondIoWebhook(t *testing.T, a *Agent, apiURL string) http.Handler {
	t.Helper()
	block := []byte("received_secret: " + webhookSecret + "\n" +
		"sent_secret: other-signing-key\napi_token: api-token\napi_url: " + apiURL + "\n")
	instances, err := channel.Build(
		context.Background(),
		map[string]channel.Decoder{"respondio": func(v any) error { return yaml.Unmarshal(block, v) }},
		core.AgentHandler{Agent: a, Log: slog.New(slog.DiscardHandler)},
	)
	if err != nil {
		t.Fatalf("channel.Build: %v", err)
	}
	for _, route := range instances[0].Channel.Routes() {
		if route.Path == "/respondio/received" {
			return route.Handler
		}
	}
	t.Fatal("the respond.io channel serves no route for incoming messages")
	return nil
}

func postWebhook(t *testing.T, h http.Handler, contactID, text string) {
	t.Helper()
	body := `{"event_type":"message.received","contact":{"id":` + contactID + `},` +
		`"message":{"message":{"type":"text","text":"` + text + `"}}}`
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, "/respondio/received", strings.NewReader(body))
	r.Header.Set("X-Webhook-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("the webhook was answered %d, want %d", w.Code, http.StatusOK)
	}
}

func TestRespondIoMessageIsAnsweredByOneTurnOfTheHarness(t *testing.T) {
	a := newAgent(t, "30s")
	api := newRespondIoAPI(t)

	postWebhook(t, respondIoWebhook(t, a.Agent, api.url), "12345", "hello harness")

	got := api.next(t)
	if got.path != "/contact/id:12345/message" {
		t.Errorf("path = %q, want the message to reach contact 12345", got.path)
	}
	// The stub streams the session id and then the text of the prompt it was sent.
	if !strings.HasSuffix(got.text, "hello harness") {
		t.Errorf("text = %q, want the reply of the harness to the prompt it received", got.text)
	}
	dir := filepath.Join(a.dataDir, directoryName("respondio:12345"))
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the turn ran in no directory of the conversation respondio:12345: %v", err)
	}
}

func TestFailedTurnSendsMomosOwnMessageToTheContact(t *testing.T) {
	a := newAgent(t, "30s", "-fail-prompt")
	api := newRespondIoAPI(t)

	postWebhook(t, respondIoWebhook(t, a.Agent, api.url), "12345", "hello harness")

	got := api.next(t)
	if got.text != "Sorry, I cannot answer your message now. Please send it again later." {
		t.Errorf("text = %q, want momo's own message about the failed turn", got.text)
	}
}
