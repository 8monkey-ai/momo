package respondio

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/8monkey-ai/momo/channel"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

func (ch respondio) Send(msg channel.Message) error {
	body, _ := json.Marshal(map[string]any{
		"message": map[string]any{"type": "text", "text": msg.Text},
	})
	baseURL := cmp.Or(ch["api_url"], "https://api.respond.io/v2")
	url := fmt.Sprintf("%s/contact/id:%s/message", baseURL, msg.ContactID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("respond.io send: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ch["api_token"])
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("respond.io send: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("respond.io send failed: %s: %q", resp.Status, respBody)
	}
	return nil
}
