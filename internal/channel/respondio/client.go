package respondio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/8monkey-ai/momo/internal/core"
)

var textURL = regexp.MustCompile(`https?://[^\s<>"']+`)

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
		for _, block := range content {
			switch block.Type {
			case "text":
				if err := c.sendTextURLs(ctx, contactID, block.Text); err != nil {
					return err
				}
			case "image", "resource_link", "resource":
				rawURL, mimeType := block.URI, block.MimeType
				if block.Type == "resource" {
					if block.Resource == nil {
						return deliveryError(block.Type)
					}
					rawURL, mimeType = block.Resource.URI, block.Resource.MimeType
				}
				parsed, err := validAttachmentURL(rawURL)
				if err != nil {
					return deliveryError(block.Type)
				}
				kind := attachmentType(mimeType)
				if mimeType == "" {
					kind = attachmentType(mime.TypeByExtension(strings.ToLower(path.Ext(parsed.Path))))
				}
				if err := c.sendAttachment(ctx, contactID, kind, parsed.String()); err != nil {
					return err
				}
			case "audio":
				return deliveryError(block.Type)
			default:
				return deliveryError(block.Type)
			}
		}
		return nil
	}
}

func deliveryError(blockType string) error {
	return fmt.Errorf("respond.io: cannot deliver %s block", blockType)
}

func attachmentType(mimeType string) string {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		mediaType = strings.ToLower(mimeType)
	}
	for _, kind := range []string{"image", "video", "audio"} {
		if strings.HasPrefix(mediaType, kind+"/") {
			return kind
		}
	}
	return "file"
}

func (c *client) sendTextURLs(ctx context.Context, contactID, text string) error {
	start := 0
	for _, match := range textURL.FindAllStringIndex(text, -1) {
		end := match[1]
		rawURL := strings.TrimRight(text[match[0]:end], `.,!?;:)]}`)
		end = match[0] + len(rawURL)
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Hostname() == "" {
			continue
		}
		mimeType := mime.TypeByExtension(strings.ToLower(path.Ext(parsed.Path)))
		if mimeType == "" {
			continue
		}
		if match[0] > start {
			if err := c.send(ctx, contactID, text[start:match[0]]); err != nil {
				return err
			}
		}
		if err := c.sendAttachment(ctx, contactID, attachmentType(mimeType), rawURL); err != nil {
			return err
		}
		start = end
	}
	if start < len(text) {
		return c.send(ctx, contactID, text[start:])
	}
	return nil
}

func (c *client) send(ctx context.Context, contactID, text string) error {
	return c.post(ctx, contactID, "message", map[string]any{
		"message": map[string]string{"type": textMessage, "text": text},
	})
}

func (c *client) sendAttachment(ctx context.Context, contactID, kind, rawURL string) error {
	return c.post(ctx, contactID, "message", map[string]any{
		"message": map[string]any{
			"type":       "attachment",
			"attachment": map[string]string{"type": kind, "url": rawURL},
		},
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
