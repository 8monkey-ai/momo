package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
)

func main() {
	cfg := loadConfig()
	srv := &server{cfg: cfg}
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
