package main

import (
	"os"

	"github.com/8monkey-ai/agent-server/channel/respondio"
)

type config struct {
	port      string
	respondio respondio.Config
}

func loadConfig() config {
	return config{
		port:      envOr("PORT", "8080"),
		respondio: respondio.ConfigFromEnv(),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
