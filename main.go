package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/8monkey-ai/momo/channel"
	"github.com/8monkey-ai/momo/channel/respondio"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	var channels []channel.WebhookReceiver
	if cfg.Channels.Respondio != nil {
		channels = append(channels, respondio.New(*cfg.Channels.Respondio))
	}

	srv := &server{channels: channels}
	httpSrv := &http.Server{Addr: fmt.Sprintf(":%d", cfg.Port), Handler: srv.routes()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("🐒 momo listening on :%d", cfg.Port)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")
	httpSrv.Close()
}
