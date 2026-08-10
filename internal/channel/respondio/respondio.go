// Package respondio receives the webhook events respond.io pushes to momo and
// replies over its REST API.
package respondio

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/core"
)

func init() {
	channel.Register("respondio", New)
}

const (
	// respond.io signs the raw body, so the whole body must be buffered before
	// it can be trusted; the limit keeps that buffer bounded.
	maxBodyBytes = 1 << 20

	signatureHeader = "X-Webhook-Signature"

	eventReceived = "message.received"
	eventSent     = "message.sent"

	textMessage = "text"
)

type settings struct {
	ReceivedSecret string `yaml:"received_secret"`
	SentSecret     string `yaml:"sent_secret"`
	ReceivedPath   string `yaml:"received_path"`
	SentPath       string `yaml:"sent_path"`
	APIToken       string `yaml:"api_token"`
	APIURL         string `yaml:"api_url"`
}

type respondio struct {
	routes []channel.Route
}

func (r respondio) Routes() []channel.Route { return r.routes }

// New configures the respond.io channel: one route per registered webhook, each
// verified with that webhook's own signing key, and one REST client shared by
// both. respond.io holds nothing that needs releasing at shutdown, so it ignores
// its lifetime.
func New(_ context.Context, decode channel.Decoder, h core.Handler) (channel.Channel, error) {
	s := settings{
		ReceivedPath: "/respondio/received",
		SentPath:     "/respondio/sent",
		APIURL:       "https://api.respond.io/v2",
	}
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.ReceivedSecret == "" || s.SentSecret == "" {
		return nil, errors.New("received_secret and sent_secret are required")
	}
	// A channel that cannot reply is a misconfiguration, not a receive-only mode.
	if s.APIToken == "" {
		return nil, errors.New("api_token is required")
	}
	api := &client{http: &http.Client{Timeout: 30 * time.Second}, url: s.APIURL, token: s.APIToken}
	return respondio{routes: []channel.Route{
		{Path: s.ReceivedPath, Handler: &webhook{secret: s.ReceivedSecret, core: h, api: api}},
		{Path: s.SentPath, Handler: &webhook{secret: s.SentSecret, core: h, api: api}},
	}}, nil
}

type webhook struct {
	secret string
	core   core.Handler
	api    *client
}

type event struct {
	EventType string `json:"event_type"`
	Contact   struct {
		ID int64 `json:"id"`
	} `json:"contact"`
	Message struct {
		Message struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"message"`
	} `json:"message"`
}

func (h *webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if !validSignature(body, r.Header.Get(signatureHeader), h.secret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	// respond.io retries events momo answers slowly, so acting on the message
	// happens after the response, detached from the request's cancellation.
	// ponytail: a dispatch still in flight is lost if momo is stopped in the same
	// instant; give the goroutine a wait group the server drains on shutdown once
	// the action is more than a log line.
	go h.dispatch(context.WithoutCancel(r.Context()), ev)
}

// dispatch ignores event types it does not act on, so respond.io can add new
// ones without momo failing.
func (h *webhook) dispatch(ctx context.Context, ev event) {
	if ev.Message.Message.Type != textMessage {
		return
	}
	m := core.Message{
		Contact: strconv.FormatInt(ev.Contact.ID, 10),
		// respond.io speaks plain text; the core's content blocks are ACP's, so the
		// conversion happens here rather than in the core.
		Content: core.Text(ev.Message.Message.Text),
	}
	switch ev.EventType {
	case eventReceived:
		// The reply is bound to the contact this event came from, so concurrent
		// webhooks each answer their own contact.
		h.core.Received(ctx, m, func(ctx context.Context, content []core.ContentBlock) error {
			return h.api.send(ctx, m.Contact, core.TextOf(content))
		})
	case eventSent:
		// An outgoing message is nothing to answer: replying to it would answer
		// momo's own replies.
		h.core.Sent(ctx, m)
	}
}

func validSignature(body []byte, header, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}
