// Package respondio implements the respond.io channel: it receives
// respond.io webhooks and translates them into channel.Messages.
package respondio

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/8monkey-ai/momo/channel"
)

// Config holds the signing keys of the two webhooks registered in respond.io.
type Config struct {
	IncomingSigningKey string
	OutgoingSigningKey string
}

type Channel struct {
	cfg Config
}

func New(cfg Config) *Channel { return &Channel{cfg: cfg} }

func (*Channel) Name() string { return "respondio" }

// Webhook returns a handler that acks events immediately and hands them to h async.
func (ch *Channel) Webhook(h channel.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		var ev webhookEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}

		var key string
		var handle func(channel.Message)
		switch ev.EventType {
		case "message.received":
			key, handle = ch.cfg.IncomingSigningKey, h.Incoming
		case "message.sent":
			key, handle = ch.cfg.OutgoingSigningKey, h.Outgoing
		}
		if key != "" && !validSignature(body, r.Header.Get("X-Webhook-Signature"), key) {
			log.Printf("rejected %q webhook: invalid signature", ev.EventType)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)

		if handle == nil {
			log.Printf("ignoring event %q", ev.EventType)
			return
		}
		go handle(channel.Message{
			ContactID: strconv.FormatInt(ev.Contact.ID, 10),
			Text:      ev.Message.Message.Text,
		})
	})
}

// Webhook payload shapes per https://developers.respond.io/docs/webhooks,
// trimmed to the fields we use.

type webhookEvent struct {
	EventType string `json:"event_type"`
	Contact   struct {
		ID int64 `json:"id"`
	} `json:"contact"`
	Message struct {
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
	} `json:"message"`
}

func validSignature(body []byte, signature, key string) bool {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}
