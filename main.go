package main

import (
	"cmp"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/8monkey-ai/momo/channel"
	"github.com/8monkey-ai/momo/channel/respondio"
)

func main() {
	port := cmp.Or(os.Getenv("PORT"), "8080")

	var channels []channel.WebhookReceiver
	respondioCfg := respondio.Config{
		IncomingSigningKey: os.Getenv("RESPOND_INCOMING_SIGNING_KEY"),
		OutgoingSigningKey: os.Getenv("RESPOND_OUTGOING_SIGNING_KEY"),
	}
	if respondioCfg != (respondio.Config{}) {
		channels = append(channels, respondio.New(respondioCfg))
	}

	srv := &server{channels: channels}
	httpSrv := &http.Server{Addr: ":" + port, Handler: srv.routes()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("🐒 momo listening on :%s", port)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")
	httpSrv.Close()
}
