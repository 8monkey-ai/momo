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

	// senderUser and senderWorkflow are the sender sources of an outgoing message
	// momo did not write: a person in the team inbox, and a respond.io workflow.
	// Every other source is momo's own reply, which the session already holds.
	senderUser     = "user"
	senderWorkflow = "workflow"

	textMessage = "text"
)

type settings struct {
	ReceivedSecret string `yaml:"received_secret"`
	SentSecret     string `yaml:"sent_secret"`
	ReceivedPath   string `yaml:"received_path"`
	SentPath       string `yaml:"sent_path"`
	APIToken       string `yaml:"api_token"`
	APIURL         string `yaml:"api_url"`
	// MomoAssigneeID is the respond.io user momo answers as. Zero leaves every
	// conversation to momo.
	MomoAssigneeID int64 `yaml:"momo_assignee_id"`
}

type respondio struct {
	routes []channel.Route
}

func (r respondio) Routes() []channel.Route { return r.routes }

// New configures the respond.io channel: one route per registered webhook, each
// verified with that webhook's own signing key. respond.io holds nothing that
// needs releasing at shutdown, so it ignores its lifetime.
func New(_ context.Context, decode channel.Decoder, h core.Handler, history core.History) (channel.Channel, error) {
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
	if s.MomoAssigneeID < 0 {
		return nil, errors.New("momo_assignee_id cannot be negative")
	}
	// An assignee id leaves the conversations of the team inbox to the people who
	// hold them, and those conversations reach the agent as history only.
	if s.MomoAssigneeID != 0 && history == nil {
		return nil, errors.New("momo_assignee_id needs the session_history_sync block")
	}
	c := &client{url: s.APIURL, token: s.APIToken, http: &http.Client{Timeout: 30 * time.Second}}
	signedWith := func(secret string) *webhook {
		return &webhook{secret: secret, core: h, history: history, client: c, momoAssigneeID: s.MomoAssigneeID}
	}
	return respondio{routes: []channel.Route{
		{Path: s.ReceivedPath, Handler: signedWith(s.ReceivedSecret)},
		{Path: s.SentPath, Handler: signedWith(s.SentSecret)},
	}}, nil
}

type webhook struct {
	secret         string
	core           core.Handler
	history        core.History
	client         *client
	momoAssigneeID int64
}

type event struct {
	EventType string `json:"event_type"`
	Contact   struct {
		ID int64 `json:"id"`
		// Assignee is absent on an unassigned conversation, which reads as the user
		// id zero: nobody holds the conversation, so momo answers it.
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
	switch ev.EventType {
	case eventReceived:
		h.received(ctx, contactID, m, ev.Contact.Assignee.ID)
	case eventSent:
		h.sent(ctx, m, ev.Sender.Source)
	}
}

// received answers the contact when the conversation is momo's, and records the
// message in the agent's session when another assignee holds it: that person
// answers, and the agent reads the message on its next turn.
func (h *webhook) received(ctx context.Context, contactID string, m core.Message, assigneeID int64) {
	// An assignee id is refused without history sync, so a conversation momo does
	// not own always has a session to be recorded in.
	if !h.owns(assigneeID) {
		h.history.RecordUser(ctx, m)
		return
	}
	// The turn puts the message in the session itself, so momo records nothing of
	// the conversations it answers.
	if err := h.core.Received(ctx, m, h.client.reply(contactID)); err != nil {
		h.report(ctx, contactID, err)
	}
}

// owns tells whether momo answers a conversation. An unassigned conversation is
// momo's, and so is one assigned to momo's own respond.io user. Without an
// assignee id every conversation is momo's, which is what a workspace without a
// team inbox wants.
func (h *webhook) owns(assigneeID int64) bool {
	return h.momoAssigneeID == 0 || assigneeID == 0 || assigneeID == h.momoAssigneeID
}

// sent records the answer a person or a workflow wrote, so the agent reads it as
// its own earlier message. momo's own replies carry another sender source and are
// in the session already; recording them would say everything twice.
func (h *webhook) sent(ctx context.Context, m core.Message, source string) {
	if h.history != nil && (source == senderUser || source == senderWorkflow) {
		h.history.RecordAssistant(ctx, m)
		return
	}
	h.core.Sent(ctx, m)
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
