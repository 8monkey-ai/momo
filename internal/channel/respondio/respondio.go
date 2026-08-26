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
	"github.com/8monkey-ai/momo/internal/extension/sessionhistorysync"
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

// recordableSources are the senders of an outgoing message the agent session does
// not hold yet: an operator in the team inbox, and a respond.io workflow. momo's
// own replies arrive with the API as their sender.
var recordableSources = map[string]bool{"user": true, "workflow": true}

type settings struct {
	ReceivedSecret string `yaml:"received_secret"`
	SentSecret     string `yaml:"sent_secret"`
	ReceivedPath   string `yaml:"received_path"`
	SentPath       string `yaml:"sent_path"`
	APIToken       string `yaml:"api_token"`
	APIURL         string `yaml:"api_url"`
	// AssigneeID is the respond.io user momo is. momo does not answer a conversation
	// another user is assigned to.
	AssigneeID int64 `yaml:"assignee_id"`
}

type respondio struct {
	routes []channel.Route
}

func (r respondio) Routes() []channel.Route { return r.routes }

// New configures the respond.io channel: one route per registered webhook, each
// verified with that webhook's own signing key. respond.io holds nothing that
// needs releasing at shutdown, so it ignores its lifetime.
func New(
	_ context.Context,
	decode channel.Decoder,
	h core.Handler,
	r sessionhistorysync.Recorder,
) (channel.Channel, error) {
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
	// Without the extension a handover loses everything the operator wrote.
	if s.AssigneeID != 0 && r == nil {
		return nil, errors.New("assignee_id requires the session_history_sync extension")
	}
	c := &client{url: s.APIURL, token: s.APIToken, http: &http.Client{Timeout: 30 * time.Second}}
	return respondio{routes: []channel.Route{
		{Path: s.ReceivedPath, Handler: &webhook{secret: s.ReceivedSecret, core: h, recorder: r, assigneeID: s.AssigneeID, client: c}},
		{Path: s.SentPath, Handler: &webhook{secret: s.SentSecret, core: h, recorder: r, assigneeID: s.AssigneeID, client: c}},
	}}, nil
}

type webhook struct {
	secret     string
	core       core.Handler
	recorder   sessionhistorysync.Recorder
	assigneeID int64
	client     *client
}

type event struct {
	EventType string `json:"event_type"`
	Contact   struct {
		ID int64 `json:"id"`
		// A pointer tells an omitted or null assignee, which is momo's conversation, from
		// a user the contact is assigned to.
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
	// The published schema does not require a sender, so momo records nothing of an
	// event without one.
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
		// A conversation someone else owns gets no answer, and its messages still reach
		// the agent session, so the next turn momo runs knows the whole conversation.
		if h.recorder != nil && h.humanOwns(ev) {
			h.record(ctx, m, sessionhistorysync.RoleUser)
			return
		}
		if err := h.core.Received(ctx, m, h.client.reply(contactID)); err != nil {
			h.report(ctx, contactID, err)
		}
	case eventSent:
		// An outgoing message momo did not send is missing from the agent session; one
		// momo sent is in it already.
		if h.recorder != nil && recordable(ev) {
			h.record(ctx, m, sessionhistorysync.RoleAssistant)
			return
		}
		h.core.Sent(ctx, m)
	}
}

// humanOwns reports whether a respond.io user other than momo is assigned to the
// contact. A contact with no assignee, and every contact of a momo that is
// nobody, belongs to momo.
func (h *webhook) humanOwns(ev event) bool {
	return h.assigneeID != 0 && ev.Contact.Assignee != nil && ev.Contact.Assignee.ID != h.assigneeID
}

// recordable reports whether an outgoing message came from someone whose words the
// agent session does not hold. The sender says who wrote the message; the assignee
// only says who owns the contact now.
func recordable(ev event) bool {
	return ev.Sender != nil && recordableSources[ev.Sender.Source]
}

// record adds a message to the agent session. A failure stays in the log: nobody
// asked for the record, so neither the contact nor the conversation is told.
func (h *webhook) record(ctx context.Context, m core.Message, role sessionhistorysync.Role) {
	if err := h.recorder.Record(ctx, m, role); err != nil {
		slog.Error("message not recorded in the agent session",
			"conversation", m.Conversation, "role", role, "error", err)
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
