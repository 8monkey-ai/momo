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

// conversations replies to each message through the channel it arrived on.
type conversations struct {
	channel channel.Channel
}

// Echo stub until the agent pipeline lands.
func (c *conversations) Incoming(msg channel.Message) {
	if err := c.channel.SendText(msg.ContactID, "You said: "+msg.Text); err != nil {
		log.Printf("contact %s: %v", msg.ContactID, err)
	}
}

// Logging stub until the agent pipeline lands.
func (c *conversations) Outgoing(msg channel.Message) {
	log.Printf("contact %s: sent message %q", msg.ContactID, msg.Text)
}
