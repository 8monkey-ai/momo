// Package respondio receives the webhook events respond.io pushes to momo and
// answers them over respond.io's REST API.
package respondio

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
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

	// Sender sources respond.io names for a message a human or a workflow sent, as
	// opposed to one momo sent through the API.
	senderUser     = "user"
	senderWorkflow = "workflow"
)

type settings struct {
	ReceivedSecret string `yaml:"received_secret"`
	SentSecret     string `yaml:"sent_secret"`
	ReceivedPath   string `yaml:"received_path"`
	SentPath       string `yaml:"sent_path"`
	APIToken       string `yaml:"api_token"`
	APIURL         string `yaml:"api_url"`
	// MomoUserID is the respond.io user momo answers as. Zero leaves every
	// conversation to momo.
	MomoUserID int64 `yaml:"momo_user_id"`
}

type respondio struct {
	routes []channel.Route
}

func (r respondio) Routes() []channel.Route { return r.routes }

// New configures the respond.io channel: one route per registered webhook, each
// verified with that webhook's own signing key. respond.io holds nothing that
// needs releasing at shutdown, so it ignores its lifetime.
func New(_ context.Context, decode channel.Decoder, h core.Handler, r core.Recorder) (channel.Channel, error) {
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
	// A channel that cannot answer is a misconfiguration, not a receive-only mode.
	if s.APIToken == "" {
		return nil, errors.New("api_token is required")
	}
	if s.MomoUserID != 0 && !r.Enabled() {
		return nil, errors.New("momo_user_id needs the session_history block, or the messages of a human assignee are lost")
	}
	c := &client{url: s.APIURL, token: s.APIToken, http: &http.Client{Timeout: 30 * time.Second}}
	return respondio{routes: []channel.Route{
		{Path: s.ReceivedPath, Handler: &webhook{secret: s.ReceivedSecret, core: h, client: c, momoUserID: s.MomoUserID, history: r}},
		{Path: s.SentPath, Handler: &webhook{secret: s.SentSecret, core: h, client: c, momoUserID: s.MomoUserID, history: r}},
	}}, nil
}

type webhook struct {
	secret     string
	core       core.Handler
	client     *client
	momoUserID int64
	history    core.Recorder
}

type event struct {
	EventType string `json:"event_type"`
	Contact   struct {
		ID       int64 `json:"id"`
		Assignee struct {
			ID int64 `json:"id"`
		} `json:"assignee"`
	} `json:"contact"`
	Sender struct {
		Source string `json:"source"`
	} `json:"sender"`
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
	contactID := strconv.FormatInt(ev.Contact.ID, 10)
	m := core.Message{
		Conversation: contactID,
		// respond.io speaks plain text; the core's content blocks are ACP's, so the
		// conversion happens here rather than in the core.
		Content: core.Text(ev.Message.Message.Text),
	}
	switch {
	case ev.EventType == eventReceived && h.humanOwns(ev):
		// A human answers this conversation; a failed record is logged where it is
		// written and never reaches the contact.
		_ = h.history.RecordUser(ctx, m)
	case ev.EventType == eventReceived:
		if err := h.core.Received(ctx, m, h.client.reply(contactID)); err != nil {
			h.report(ctx, contactID, err)
		}
	case ev.EventType == eventSent && writtenInTheWorkspace(ev):
		_ = h.history.RecordAssistant(ctx, m)
	case ev.EventType == eventSent:
		// What is left is momo's own reply; nothing answers it.
		h.core.Sent(ctx, m)
	}
}

// humanOwns treats an unassigned conversation as momo's, and every conversation
// as momo's while no assignee id is configured.
func (h *webhook) humanOwns(ev event) bool {
	if h.momoUserID == 0 {
		return false
	}
	assignee := ev.Contact.Assignee.ID
	return assignee != 0 && assignee != h.momoUserID
}

// writtenInTheWorkspace excludes momo's own replies, which carry another sender
// source.
func writtenInTheWorkspace(ev event) bool {
	return ev.Sender.Source == senderUser || ev.Sender.Source == senderWorkflow
}

// report tells the operators of the conversation that the message got no reply.
// The comment is internal, so a failed turn never reaches the contact.
func (h *webhook) report(ctx context.Context, contactID string, turn error) {
	if err := h.client.comment(ctx, contactID, "momo could not answer this message: "+turn.Error()); err != nil {
		slog.Error("failed turn not reported", "contact", contactID, "turn", turn, "error", err)
	}
}

func validSignature(body []byte, header, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}
