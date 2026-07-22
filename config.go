package main

import (
	"cmp"
	"os"

	"github.com/8monkey-ai/agent-server/channel/respondio"
)

type config struct {
	port      string
	respondio respondio.Config
}

func loadConfig() config {
	return config{
		port:      cmp.Or(os.Getenv("PORT"), "8080"),
		respondio: respondio.ConfigFromEnv(),
	}
}
