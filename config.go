package main

import "os"

type config struct {
	port               string
	incomingSigningKey string
	outgoingSigningKey string
}

func loadConfig() config {
	return config{
		port:               envOr("PORT", "8080"),
		incomingSigningKey: os.Getenv("RESPOND_INCOMING_SIGNING_KEY"),
		outgoingSigningKey: os.Getenv("RESPOND_OUTGOING_SIGNING_KEY"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
