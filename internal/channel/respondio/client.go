package respondio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// client sends replies over respond.io's REST API. One client serves every
// contact: the contact is the caller's, so the same client is shared by concurrent
// webhooks.
type client struct {
	http  *http.Client
	url   string
	token string
}

type outgoing struct {
	Message outgoingMessage `json:"message"`
}

type outgoingMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// send posts one text message to a contact. Text nothing can be made of is
// nothing to say, not a failure, and no call is issued for it.
func (c *client) send(ctx context.Context, contactID, text string) error {
	if text == "" {
		return nil
	}
	body, err := json.Marshal(outgoing{Message: outgoingMessage{Type: textMessage, Text: text}})
	if err != nil {
		return fmt.Errorf("respond.io: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url+"/contact/id:"+contactID+"/message", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("respond.io: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("respond.io: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		// An error body momo did not ask for can be any size, so only its opening is
		// read into the message.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("respond.io: status %d: %s", resp.StatusCode, detail)
	}
	return nil
}
