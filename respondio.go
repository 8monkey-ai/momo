package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Webhook payload shapes per https://developers.respond.io/docs/webhooks
// (New Incoming Message / New Outgoing Message), trimmed to the fields we use.

type webhookEvent struct {
	EventType string       `json:"event_type"`
	Contact   contact      `json:"contact"`
	Message   eventMessage `json:"message"`
}

type contact struct {
	ID       int64     `json:"id"`
	Assignee *assignee `json:"assignee"` // nil when the conversation is unassigned
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

// validSignature checks a webhook's X-Webhook-Signature header: base64 of
// HMAC-SHA256 over the raw body with the webhook's signing key.
func validSignature(body []byte, signature, key string) bool {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}

type respondClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newRespondClient(baseURL, token string) *respondClient {
	return &respondClient{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *respondClient) sendText(contactID int64, text string) error {
	return c.send(contactID, map[string]any{"type": "text", "text": text})
}

func (c *respondClient) sendAttachment(contactID int64, attType, url string) error {
	return c.send(contactID, map[string]any{
		"type":       "attachment",
		"attachment": map[string]string{"type": attType, "url": url},
	})
}

func (c *respondClient) send(contactID int64, message map[string]any) error {
	body, _ := json.Marshal(map[string]any{"message": message})
	url := fmt.Sprintf("%s/contact/id:%d/message", c.baseURL, contactID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("respond.io send failed: %s: %s", resp.Status, respBody)
	}
	return nil
}

func (c *respondClient) download(url string) ([]byte, error) {
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("download failed: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 50<<20))
}
