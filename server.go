package main

import (
	"log"
	"net/http"

	"github.com/8monkey-ai/momo/channel"
	"github.com/8monkey-ai/momo/channel/respondio"
)

type server struct {
	cfg config
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	if s.cfg.respondio.Configured() {
		mux.Handle("POST /webhook/respondio", respondio.Webhook(s.cfg.respondio, s))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// Incoming will prompt the contact's agent and deliver its reply;
// until that pipeline lands, it only logs.
func (s *server) Incoming(msg channel.Message) {
	log.Printf("contact %s: received message %q", msg.ContactID, msg.Text)
}

// Outgoing will record operator replies into the agent's context;
// until that pipeline lands, it only logs.
func (s *server) Outgoing(msg channel.Message) {
	log.Printf("contact %s: sent message %q", msg.ContactID, msg.Text)
}
