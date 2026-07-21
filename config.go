package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// envReader collects parse errors so loadConfig reports them all at once.
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
	port string

	apiToken           string
	apiBaseURL         string
	incomingSigningKey string
	outgoingSigningKey string
	aiAssigneeID       int64

	agentCmd        string // overridden only in tests
	dataDir         string
	contactTemplate string

	typingPerWord time.Duration
}

// contactDir returns the contact's working directory, creating it if missing.
func (c config) contactDir(contactID int64) (string, error) {
	dir := filepath.Join(c.dataDir, fmt.Sprint(contactID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
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
		typingPerWord:      time.Duration(env.int("TYPING_DELAY_MS_PER_WORD", 1000)) * time.Millisecond,
	}, env.err
}
