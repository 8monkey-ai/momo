package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// Webhook payload shapes per https://developers.respond.io/docs/webhooks,
// trimmed to the fields we use.

type webhookEvent struct {
	EventType string       `json:"event_type"`
	Contact   contact      `json:"contact"`
	Message   eventMessage `json:"message"`
}

type contact struct {
	ID int64 `json:"id"`
	// Unassigned arrives as an object of nulls; the value type folds every
	// unassigned shape into ID == 0.
	Assignee assignee `json:"assignee"`
}

type assignee struct {
	ID int64 `json:"id"`
}

type eventMessage struct {
	Message messageContent `json:"message"`
}

type messageContent struct {
	Type       string      `json:"type"`
	Text       string      `json:"text"`
	Attachment *attachment `json:"attachment"`
}

type attachment struct {
	Type     string `json:"type"` // image, video, audio, file
	URL      string `json:"url"`
	FileName string `json:"fileName"`
	Mime     string `json:"mime"`
}

// validSignature checks X-Webhook-Signature: base64 HMAC-SHA256 of the raw
// body with the webhook's signing key.
func validSignature(body []byte, signature, key string) bool {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}
