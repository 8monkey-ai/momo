// Package respondio implements the respond.io channel.
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

func init() {
	channel.Register("respondio", func(settings map[string]string) channel.Channel {
		return respondio(settings)
	})
}

type respondio map[string]string

func (ch respondio) Start(h channel.Handler, mux *http.ServeMux) {
	const route = "POST /respondio"
	mux.Handle(route, ch.webhook(h))
	log.Printf("respondio: listening on %s", route)
}

func (ch respondio) webhook(h channel.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
			key = ch["incoming_signing_key"]
			handle = func(msg channel.Message) { h.Incoming(msg, ch.Send) }
		case "message.sent":
			key = ch["outgoing_signing_key"]
			handle = h.Outgoing
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
	}
}

// Payload shapes per https://developers.respond.io/docs/webhooks, trimmed to
// the fields we use.
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
