package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Webhook payload shapes per https://developers.respond.io/docs/webhooks
// (New Incoming Message / New Outgoing Message).

type webhookEvent struct {
	EventType string       `json:"event_type"`
	EventID   string       `json:"event_id"`
	Contact   contact      `json:"contact"`
	Message   eventMessage `json:"message"`
}

type contact struct {
	ID        int64  `json:"id"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Phone     string `json:"phone"`
	Email     string `json:"email"`
}

type eventMessage struct {
	MessageID int64          `json:"messageId"`
	ChannelID int64          `json:"channelId"`
	Traffic   string         `json:"traffic"`
	Timestamp int64          `json:"timestamp"`
	Message   messageContent `json:"message"`
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

type respondClient struct {
	baseURL string
	token   string
	http    *http.Client

	mu   sync.Mutex
	sent map[int64]time.Time // messageIds we produced, for the outgoing-webhook echo filter
}

func newRespondClient(baseURL, token string) *respondClient {
	return &respondClient{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
		sent:    make(map[int64]time.Time),
	}
}

// sendText posts a text message to a contact and records the returned
// messageId so the outgoing webhook can recognize our own messages.
func (c *respondClient) sendText(contactID int64, text string) error {
	body, _ := json.Marshal(map[string]any{
		"message": map[string]string{"type": "text", "text": text},
	})
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
	var out struct {
		MessageID int64 `json:"messageId"`
	}
	if err := json.Unmarshal(respBody, &out); err == nil && out.MessageID != 0 {
		c.rememberSent(out.MessageID)
	}
	return nil
}

func (c *respondClient) rememberSent(messageID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for id, t := range c.sent {
		if now.Sub(t) > time.Hour {
			delete(c.sent, id)
		}
	}
	c.sent[messageID] = now
}

func (c *respondClient) wasSentByUs(messageID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.sent[messageID]
	return ok
}

// download fetches an attachment URL into memory.
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
