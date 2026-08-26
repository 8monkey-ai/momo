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

	// respond.io names the sender of an outgoing message in source. A user is an
	// operator in the team inbox and a workflow is an automation of the workspace;
	// both write text momo did not write.
	sourceUser     = "user"
	sourceWorkflow = "workflow"

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
func New(_ context.Context, decode channel.Decoder, h core.Handler, rec channel.Recorder) (channel.Channel, error) {
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
	// Naming momo's user is what hands a conversation to a human, and without the
	// extension momo has nowhere to keep what that human writes.
	if s.MomoAssigneeID != 0 && rec == nil {
		return nil, errors.New("momo_assignee_id requires the session_history_sync block")
	}
	c := &client{url: s.APIURL, token: s.APIToken, http: &http.Client{Timeout: 30 * time.Second}}
	hook := func(secret string) *webhook {
		return &webhook{secret: secret, core: h, client: c, recorder: rec, momoAssignee: s.MomoAssigneeID}
	}
	return respondio{routes: []channel.Route{
		{Path: s.ReceivedPath, Handler: hook(s.ReceivedSecret)},
		{Path: s.SentPath, Handler: hook(s.SentSecret)},
	}}, nil
}

type webhook struct {
	secret string
	core   core.Handler
	client *client
	// recorder is nil only when momoAssignee is zero, so a handed-over conversation
	// always has one.
	recorder     channel.Recorder
	momoAssignee int64
}

type event struct {
	EventType string `json:"event_type"`
	Contact   struct {
		ID       int64 `json:"id"`
		Assignee *struct {
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
		h.received(ctx, m, ev)
	case eventSent:
		h.sent(ctx, m, ev)
	}
}

// received answers the contact with one turn, unless another respond.io user owns
// the conversation: that user is answering, so the contact's text belongs in the
// agent's session and nowhere else.
func (h *webhook) received(ctx context.Context, m core.Message, ev event) {
	if h.handedOver(ev) {
		h.recorder.RecordUser(ctx, m)
		return
	}
	if err := h.core.Received(ctx, m, h.client.reply(m.Conversation)); err != nil {
		h.report(ctx, m.Conversation, err)
	}
}

// sent keeps what a respond.io user or a workflow wrote in the agent's session.
// Every other outgoing message is momo's own reply, which the session holds.
func (h *webhook) sent(ctx context.Context, m core.Message, ev event) {
	source := ev.Message.Source
	if h.recorder != nil && (source == sourceUser || source == sourceWorkflow) {
		h.recorder.RecordAssistant(ctx, m)
		return
	}
	h.core.Sent(ctx, m)
}

// handedOver reports that another respond.io user owns the conversation. An
// unassigned conversation belongs to momo, and so does every conversation while
// the operator names no user for momo.
func (h *webhook) handedOver(ev event) bool {
	return h.momoAssignee != 0 && ev.Contact.Assignee != nil && ev.Contact.Assignee.ID != h.momoAssignee
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
