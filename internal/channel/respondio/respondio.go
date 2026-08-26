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

	// sourceUser and sourceWorkflow are the outgoing senders momo did not produce:
	// what they wrote belongs in the agent's session as an assistant message.
	// respond.io's schema does not list every source it may send, so every other
	// source follows the ordinary outgoing flow.
	sourceUser     = "user"
	sourceWorkflow = "workflow"
)

type settings struct {
	ReceivedSecret string `yaml:"received_secret"`
	SentSecret     string `yaml:"sent_secret"`
	ReceivedPath   string `yaml:"received_path"`
	SentPath       string `yaml:"sent_path"`
	APIToken       string `yaml:"api_token"`
	APIURL         string `yaml:"api_url"`
	// MomoUserID is the respond.io user momo is. Zero means momo owns every
	// conversation, whoever it is assigned to.
	MomoUserID int64 `yaml:"momo_user_id"`
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
	// Without the session history sync, a conversation a human owns would be
	// answered by momo as well, which is what the assignee id is set to prevent.
	if s.MomoUserID != 0 && history == nil {
		return nil, errors.New("momo_user_id requires the session-history-sync extension")
	}
	c := &client{url: s.APIURL, token: s.APIToken, http: &http.Client{Timeout: 30 * time.Second}}
	return respondio{routes: []channel.Route{
		{Path: s.ReceivedPath, Handler: &webhook{
			secret: s.ReceivedSecret, core: h, client: c, history: history, momoUserID: s.MomoUserID}},
		{Path: s.SentPath, Handler: &webhook{
			secret: s.SentSecret, core: h, client: c, history: history, momoUserID: s.MomoUserID}},
	}}, nil
}

type webhook struct {
	secret     string
	core       core.Handler
	client     *client
	history    core.History
	momoUserID int64
}

type event struct {
	EventType string `json:"event_type"`
	Contact   struct {
		ID       int64 `json:"id"`
		Assignee struct {
			ID int64 `json:"id"`
		} `json:"assignee"`
	} `json:"contact"`
	Message struct {
		Source  string `json:"source"`
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
		// The text reaches the session so the agent knows what was said, and the human
		// who owns the conversation answers alone.
		if h.humanOwns(ev.Contact.Assignee.ID) {
			h.history.RecordUser(ctx, m)
			return
		}
		if err := h.core.Received(ctx, m, h.client.reply(contactID)); err != nil {
			h.report(ctx, contactID, err)
		}
	case eventSent:
		// What a person or a workflow sent is an answer the agent never produced, so the
		// session would lose it. momo's own reply is already in the session, written by
		// the turn that produced it.
		if h.history != nil && (ev.Message.Source == sourceUser || ev.Message.Source == sourceWorkflow) {
			h.history.RecordAssistant(ctx, m)
			return
		}
		h.core.Sent(ctx, m)
	}
}

// An unassigned conversation is momo's, and so is every conversation when the
// operator configured no assignee id for momo. A configured id comes with the
// history, so a conversation a human owns is always recordable.
func (h *webhook) humanOwns(assigneeID int64) bool {
	return h.momoUserID != 0 && assigneeID != 0 && assigneeID != h.momoUserID
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
