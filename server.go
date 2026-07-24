package main

import (
	"log"
	"net/http"

	"github.com/8monkey-ai/momo/channel"
)

type server struct {
	channels map[string]channel.Channel
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	for name, ch := range s.channels {
		if wh, ok := ch.(channel.WebhookHandler); ok {
			mux.Handle("POST /webhook/"+name, wh.Webhook(&conversations{channel: ch}))
		}
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// conversations handles one channel's messages, replying through that
// same channel.
type conversations struct {
	channel channel.Channel
}

// Incoming will prompt the contact's agent and deliver its reply;
// until the agent pipeline lands, it echoes the message back.
func (c *conversations) Incoming(msg channel.Message) {
	if err := c.channel.SendText(msg.ContactID, "You said: "+msg.Text); err != nil {
		log.Printf("contact %s: %v", msg.ContactID, err)
	}
}

// Outgoing will record operator replies into the agent's context;
// until that pipeline lands, it only logs.
func (c *conversations) Outgoing(msg channel.Message) {
	log.Printf("contact %s: sent message %q", msg.ContactID, msg.Text)
}
