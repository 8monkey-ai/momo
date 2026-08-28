package respondio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/8monkey-ai/momo/internal/core"
)

var (
	errAttachmentTooLarge = errors.New("attachment is too large")
	errInvalidRedirect    = errors.New("invalid attachment redirect")
)

type attachment struct {
	Type     string `json:"type"`
	URL      string `json:"url"`
	Name     string `json:"name"`
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType"`
}

func (h *webhook) attachmentContent(ctx context.Context, conversation string, attachments []attachment) []core.ContentBlock {
	content := make([]core.ContentBlock, 0, len(attachments))
	for _, item := range attachments {
		content = append(content, h.attachmentBlock(ctx, conversation, item))
	}
	return content
}

func (h *webhook) attachmentBlock(ctx context.Context, conversation string, item attachment) core.ContentBlock {
	name := first(item.Name, item.FileName)
	response, parsed, reason := h.fetchAttachment(ctx, item.URL)
	if reason != "" {
		return unavailable(nameFromURL(name, parsed), reason)
	}
	defer func() { _ = response.Body.Close() }()

	finalURL := response.Request.URL
	headerName := contentDispositionName(response.Header.Get("Content-Disposition"))
	proposedName := safeDisplayName(first(name, headerName, path.Base(finalURL.Path)))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return unavailable(proposedName, "download failed")
	}
	if response.ContentLength > h.maxAttachmentBytes {
		return unavailable(proposedName, "too large")
	}

	mediaType := first(item.MimeType, headerMediaType(response.Header.Get("Content-Type")), mime.TypeByExtension(strings.ToLower(path.Ext(finalURL.Path))), "application/octet-stream")
	if h.files == nil {
		return unavailable(proposedName, "storage unavailable")
	}
	limited := &attachmentReader{reader: response.Body, remaining: h.maxAttachmentBytes}
	safeName, uri, err := h.files.Save(ctx, conversation, proposedName, limited)
	if err != nil {
		return unavailable(proposedName, attachmentSaveFailure(err, limited))
	}
	return core.ContentBlock{Type: "resource_link", URI: uri, Name: safeName, MimeType: mediaType, Size: limited.read}
}

func (h *webhook) fetchAttachment(ctx context.Context, rawURL string) (*http.Response, *url.URL, string) {
	parsed, err := validAttachmentURL(rawURL)
	if err != nil {
		return nil, parsed, "invalid address"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, parsed, "invalid address"
	}
	response, err := h.downloadClient().Do(request)
	if err != nil {
		return nil, parsed, attachmentDownloadFailure(err)
	}
	return response, parsed, ""
}

func attachmentDownloadFailure(err error) string {
	if errors.Is(err, errInvalidRedirect) {
		return "invalid redirect"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "download cancelled"
	}
	return "download failed"
}

func attachmentSaveFailure(err error, reader *attachmentReader) string {
	if errors.Is(err, errAttachmentTooLarge) {
		return "too large"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "download cancelled"
	}
	if reader.readErr != nil {
		return "download failed"
	}
	return "storage failed"
}

func (h *webhook) downloadClient() *http.Client {
	client := *h.client.http
	original := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if _, err := validAttachmentURL(req.URL.String()); err != nil {
			return errInvalidRedirect
		}
		if original != nil {
			return original(req, via)
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return &client
}

func validAttachmentURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return parsed, errors.New("attachment URL must be absolute HTTP(S)")
	}
	return parsed, nil
}

type attachmentReader struct {
	reader    io.Reader
	remaining int64
	read      int64
	readErr   error
}

func (r *attachmentReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			r.readErr = errAttachmentTooLarge
			return 0, errAttachmentTooLarge
		}
		if err != io.EOF {
			r.readErr = err
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	r.read += int64(n)
	if err != nil && err != io.EOF {
		r.readErr = err
	}
	return n, err
}

func contentDispositionName(value string) string {
	_, params, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return params["filename"]
}

func headerMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return mediaType
}

func nameFromURL(name string, parsed *url.URL) string {
	if name == "" && parsed != nil {
		name = path.Base(parsed.Path)
	}
	return safeDisplayName(name)
}

func safeDisplayName(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "attachment"
	}
	return name
}

func unavailable(name, reason string) core.ContentBlock {
	return core.ContentBlock{Type: "text", Text: fmt.Sprintf("Attachment %q is unavailable: %s.", safeDisplayName(name), reason)}
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
