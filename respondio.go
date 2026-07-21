package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// respondClient calls the respond.io REST API (sending messages, downloading
// attachments).
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
