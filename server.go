package main

import (
	"log"
	"net/http"

	"github.com/8monkey-ai/momo/channel"
)

type server struct {
	channels []channel.Channel
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	for _, ch := range s.channels {
		ch.Start(s, mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (s *server) Incoming(msg channel.Message, reply channel.Reply) {
	echo := channel.Message{
		ContactID: msg.ContactID,
		Text:      "You said: " + msg.Text,
	}
	if err := reply(echo); err != nil {
		log.Printf("contact %s: %v", msg.ContactID, err)
	}
}

func (s *server) Outgoing(msg channel.Message) {
	log.Printf("contact %s: sent message %q", msg.ContactID, msg.Text)
}
