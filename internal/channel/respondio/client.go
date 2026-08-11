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
	body, err := json.Marshal(map[string]any{
		"message": map[string]string{"type": textMessage, "text": text},
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/contact/id:%s/message", c.url, contactID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("respond.io: sending to contact %s: %w", contactID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		// The body explains the refusal; the bound keeps an HTML error page out of
		// the log.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("respond.io: sending to contact %s: status %d: %s", contactID, resp.StatusCode, detail)
	}
	return nil
}
