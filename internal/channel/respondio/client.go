package respondio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/8monkey-ai/momo/internal/core"
)

// client sends messages through respond.io's REST API. One is shared by every
// contact: what a reply needs of its own is the contact id, and that is captured
// when the message arrives.
type client struct {
	url   string
	token string
	http  *http.Client
}

// reply answers the contact the incoming event came from.
func (c *client) reply(contactID string) core.Reply {
	return func(ctx context.Context, content []core.ContentBlock) error {
		// respond.io carries plain text, so the blocks it cannot carry are dropped
		// here; a reply that is left with nothing to say makes no API call.
		text := core.TextOf(content)
		if text == "" {
			return nil
		}
		return c.send(ctx, contactID, text)
	}
}

func (c *client) send(ctx context.Context, contactID, text string) error {
	return c.post(ctx, contactID, "message", map[string]any{
		"message": map[string]string{"type": textMessage, "text": text},
	})
}

// comment adds an internal note to the contact's conversation. Only the
// operators of the workspace read it; the contact does not.
func (c *client) comment(ctx context.Context, contactID, text string) error {
	return c.post(ctx, contactID, "comment", map[string]string{"text": text})
}

func (c *client) post(ctx context.Context, contactID, resource string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/contact/id:%s/%s", c.url, contactID, resource)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("respond.io: posting a %s for contact %s: %w", resource, contactID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		// The body explains the refusal; the bound keeps an HTML error page out of
		// the log.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("respond.io: posting a %s for contact %s: status %d: %s", resource, contactID, resp.StatusCode, detail)
	}
	return nil
}
