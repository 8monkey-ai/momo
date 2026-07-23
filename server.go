package main

import (
	"log"
	"net/http"

	"github.com/8monkey-ai/momo/channel"
)

type server struct {
	channels []channel.WebhookReceiver
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	for _, ch := range s.channels {
		mux.Handle("POST /webhook/"+ch.Name(), ch.Webhook(s))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// Incoming is a logging stub until the agent pipeline lands.
func (s *server) Incoming(msg channel.Message) {
	log.Printf("contact %s: received message %q", msg.ContactID, msg.Text)
}

// Outgoing is a logging stub until the agent pipeline lands.
func (s *server) Outgoing(msg channel.Message) {
	log.Printf("contact %s: sent message %q", msg.ContactID, msg.Text)
}
