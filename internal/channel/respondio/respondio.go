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
	"github.com/8monkey-ai/momo/internal/extension/sessionhistory"
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

	// senderUser is an outgoing message a human operator wrote, and senderWorkflow
	// one a respond.io workflow sent. Both belong in the agent's session; momo's own
	// API replies do not, and they carry another source.
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
	// AssigneeID is the respond.io user momo is. Zero keeps every conversation with
	// momo, whoever the contact is assigned to.
	AssigneeID int64 `yaml:"assignee_id"`
}

type respondio struct {
	routes []channel.Route
}

func (r respondio) Routes() []channel.Route { return r.routes }

// New configures the respond.io channel: one route per registered webhook, each
// verified with that webhook's own signing key. respond.io holds nothing that
// needs releasing at shutdown, so it ignores its lifetime.
func New(_ context.Context, decode channel.Decoder, h core.Handler, r sessionhistory.Recorder) (channel.Channel, error) {
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
	// Without the extension a handover loses the part of the conversation the
	// operator handled, so momo refuses to enter one.
	if s.AssigneeID != 0 && r == nil {
		return nil, errors.New("assignee_id requires the extensions.session_history block")
	}
	c := &client{url: s.APIURL, token: s.APIToken, http: &http.Client{Timeout: 30 * time.Second}}
	hook := func(secret string) *webhook {
		return &webhook{secret: secret, core: h, client: c, assigneeID: s.AssigneeID, history: r}
	}
	return respondio{routes: []channel.Route{
		{Path: s.ReceivedPath, Handler: hook(s.ReceivedSecret)},
		{Path: s.SentPath, Handler: hook(s.SentSecret)},
	}}, nil
}

type webhook struct {
	secret     string
	core       core.Handler
	client     *client
	assigneeID int64
	// history is nil while the session-history extension is disabled.
	history sessionhistory.Recorder
}

type event struct {
	EventType string `json:"event_type"`
	Contact   struct {
		ID int64 `json:"id"`
		// A pointer tells an unassigned contact, which is momo's, from one assigned
		// to a user: respond.io omits the member or sends null for it.
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
	// The published schema does not require a sender, so an outgoing message may
	// arrive without one.
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
		// While an operator holds the contact, the contact's message goes to the
		// session and no turn starts, so the agent knows what was said to it.
		if h.humanOwned(ev) {
			h.record(ctx, m, sessionhistory.RoleUser)
			return
		}
		if err := h.core.Received(ctx, m, h.client.reply(contactID)); err != nil {
			h.report(ctx, contactID, err)
		}
	case eventSent:
		// An operator's or a workflow's message is the assistant's part of the
		// conversation; momo's own reply is already in the session.
		if h.history != nil && recordableSender(ev) {
			h.record(ctx, m, sessionhistory.RoleAssistant)
			return
		}
		h.core.Sent(ctx, m)
	}
}

// humanOwned answers whether a user other than momo holds the contact. An
// unassigned contact is momo's, and so is every contact while assignee_id is
// unset.
func (h *webhook) humanOwned(ev event) bool {
	return h.assigneeID != 0 && ev.Contact.Assignee != nil && ev.Contact.Assignee.ID != h.assigneeID
}

// recordableSender answers whether the sender of an outgoing message is one the
// agent's session is missing. The sender says who wrote the message; the assignee
// only says who holds the contact now.
func recordableSender(ev event) bool {
	if ev.Sender == nil {
		return false
	}
	return ev.Sender.Source == senderUser || ev.Sender.Source == senderWorkflow
}

// record logs a failed record and does nothing else: nothing about it belongs on
// the conversation, and nothing at all reaches the contact.
func (h *webhook) record(ctx context.Context, m core.Message, role sessionhistory.Role) {
	if err := h.history.Record(ctx, m, role); err != nil {
		slog.Error("message not recorded", "contact", m.Conversation, "role", role, "error", err)
	}
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
