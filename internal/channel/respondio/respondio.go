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

	// Sources of an outgoing message momo records: an operator writes one as "user",
	// and a respond.io workflow as "workflow". momo's own replies carry another
	// source, and the agent session already holds them.
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
	// AssigneeID is the respond.io user momo answers as. Zero keeps every
	// conversation with momo, whoever holds it.
	AssigneeID int64 `yaml:"assignee_id"`
}

type respondio struct {
	routes []channel.Route
}

func (r respondio) Routes() []channel.Route { return r.routes }

// New configures the respond.io channel: one route per registered webhook, each
// verified with that webhook's own signing key. respond.io holds nothing that
// needs releasing at shutdown, so it ignores its lifetime.
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
	// A channel that cannot answer is a misconfiguration, not a receive-only mode.
	if s.APIToken == "" {
		return nil, errors.New("api_token is required")
	}
	c := &client{url: s.APIURL, token: s.APIToken, http: &http.Client{Timeout: 30 * time.Second}}
	return respondio{routes: []channel.Route{
		{Path: s.ReceivedPath, Handler: &webhook{secret: s.ReceivedSecret, core: h, client: c, assigneeID: s.AssigneeID}},
		{Path: s.SentPath, Handler: &webhook{secret: s.SentSecret, core: h, client: c, assigneeID: s.AssigneeID}},
	}}, nil
}

type webhook struct {
	secret     string
	core       core.Handler
	client     *client
	assigneeID int64
}

type event struct {
	EventType string `json:"event_type"`
	Contact   struct {
		ID int64 `json:"id"`
		// A pointer tells an unassigned contact, which respond.io reports as an
		// omitted or null member, from one an assignee holds.
		Assignee *struct {
			ID int64 `json:"id"`
		} `json:"assignee"`
	} `json:"contact"`
	Message struct {
		Message struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"message"`
	} `json:"message"`
	// The official schema does not require a sender, so an event without one is not
	// malformed.
	Sender *struct {
		Source string `json:"source"`
	} `json:"sender"`
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
	switch ev.EventType {
	case eventReceived:
		if h.humanOwned(ev) {
			// A human operator answers this conversation, so momo follows it and stays
			// silent.
			h.core.Record(ctx, m, core.RoleUser)
			return
		}
		if err := h.core.Received(ctx, m, h.client.reply(contactID)); err != nil {
			h.report(ctx, contactID, err)
		}
	case eventSent:
		if recordable(ev) {
			h.core.Record(ctx, m, core.RoleAssistant)
			return
		}
		// What is left is momo's own reply, which the agent session already holds.
		h.core.Sent(ctx, m)
	}
}

// humanOwned tells whether somebody other than momo holds the contact.
func (h *webhook) humanOwned(ev event) bool {
	return h.assigneeID != 0 && ev.Contact.Assignee != nil && ev.Contact.Assignee.ID != h.assigneeID
}

// recordable tells whether the workspace, and not momo's API call, wrote an
// outgoing message. The sender says who wrote it; the assignee says only who holds
// the contact now.
func recordable(ev event) bool {
	if ev.Sender == nil {
		return false
	}
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
