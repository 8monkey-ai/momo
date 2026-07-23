package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/8monkey-ai/momo/channel"
)

func main() {
	cfg := loadConfig()

	var channels []channel.WebhookReceiver
	if cfg.respondio.Configured() {
		channels = append(channels, cfg.respondio)
	}

	srv := &server{channels: channels}
	httpSrv := &http.Server{Addr: ":" + cfg.port, Handler: srv.routes()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("🐒 momo listening on :%s", cfg.port)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")
	httpSrv.Close()
}
