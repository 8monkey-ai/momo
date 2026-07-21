package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

type server struct {
	cfg config
}

func newServer(cfg config) *server {
	return &server{cfg: cfg}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", s.handleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// handleWebhook acks immediately and processes the event async. The webhook
// response is only a delivery ack — replies go through the REST API — so
// there is nothing to gain by holding the request open, and a response
// slower than respond.io's timeout would read as failed delivery.
func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
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
	var handle func(webhookEvent)
	switch ev.EventType {
	case "message.received":
		key, handle = s.cfg.incomingSigningKey, s.handleIncoming
	case "message.sent":
		key, handle = s.cfg.outgoingSigningKey, s.handleOutgoing
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
	go handle(ev)
}

// handleIncoming will prompt the contact's agent and deliver its reply;
// until that pipeline lands, it only logs.
func (s *server) handleIncoming(ev webhookEvent) {
	log.Printf("contact %d: received %s message %q", ev.Contact.ID, ev.Message.Message.Type, ev.Message.Message.Text)
}

// handleOutgoing will record operator replies into the agent's context;
// until that pipeline lands, it only logs.
func (s *server) handleOutgoing(ev webhookEvent) {
	log.Printf("contact %d: sent %s message %q", ev.Contact.ID, ev.Message.Message.Type, ev.Message.Message.Text)
}
