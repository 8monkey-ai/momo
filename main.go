package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	port            string
	respondToken    string
	respondBaseURL  string
	agentCmd        []string
	dataDir         string
	typingPerChar   time.Duration
	outgoingCommand string
}

func loadConfig() (config, error) {
	cfg := config{
		port:            envOr("PORT", "8080"),
		respondToken:    os.Getenv("RESPOND_API_TOKEN"),
		respondBaseURL:  envOr("RESPOND_API_URL", "https://api.respond.io/v2"),
		agentCmd:        strings.Fields(os.Getenv("AGENT_CMD")),
		dataDir:         envOr("DATA_DIR", "./data"),
		outgoingCommand: os.Getenv("OUTGOING_COMMAND"),
	}
	if cfg.respondToken == "" {
		return cfg, fmt.Errorf("RESPOND_API_TOKEN is required")
	}
	if len(cfg.agentCmd) == 0 {
		return cfg, fmt.Errorf("AGENT_CMD is required (e.g. \"claude-code-acp\" or \"gemini --experimental-acp\")")
	}
	ms, err := strconv.Atoi(envOr("TYPING_DELAY_MS_PER_CHAR", "30"))
	if err != nil {
		return cfg, fmt.Errorf("TYPING_DELAY_MS_PER_CHAR: %w", err)
	}
	cfg.typingPerChar = time.Duration(ms) * time.Millisecond
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(cfg.dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	srv := newServer(cfg)
	log.Printf("agent-server listening on :%s (harness: %s)", cfg.port, strings.Join(cfg.agentCmd, " "))
	log.Fatal(http.ListenAndServe(":"+cfg.port, srv.routes()))
}
