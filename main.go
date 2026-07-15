package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// envReader reads env vars with defaults, collecting parse errors so
// loadConfig can report them all at once.
type envReader struct{ err error }

func (r *envReader) str(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (r *envReader) int(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		r.err = errors.Join(r.err, fmt.Errorf("%s: %w", key, err))
	}
	return n
}

type config struct {
	// http server
	port string

	// respond.io
	apiToken           string
	apiBaseURL         string
	incomingSigningKey string
	outgoingSigningKey string
	aiAssigneeID       int64

	// harness
	agentCmd        string // the pi-acp harness; overridden only in tests
	dataDir         string
	contactTemplate string

	// reply delivery
	typingPerChar time.Duration
}

func loadConfig() (config, error) {
	var env envReader
	return config{
		port:               env.str("PORT", "8080"),
		apiToken:           os.Getenv("RESPOND_API_TOKEN"),
		apiBaseURL:         env.str("RESPOND_API_URL", "https://api.respond.io/v2"),
		incomingSigningKey: os.Getenv("RESPOND_INCOMING_SIGNING_KEY"),
		outgoingSigningKey: os.Getenv("RESPOND_OUTGOING_SIGNING_KEY"),
		aiAssigneeID:       env.int("RESPOND_AI_ASSIGNEE_ID", 0),
		agentCmd:           "pi-acp",
		dataDir:            env.str("DATA_DIR", "./data"),
		contactTemplate:    os.Getenv("CONTACT_TEMPLATE"),
		typingPerChar:      time.Duration(env.int("TYPING_DELAY_MS_PER_CHAR", 30)) * time.Millisecond,
	}, env.err
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	srv := newServer(cfg)
	log.Printf("🐒 agent-server listening on :%s", cfg.port)
	log.Fatal(http.ListenAndServe(":"+cfg.port, srv.routes()))
}
