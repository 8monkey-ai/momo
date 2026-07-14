package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	acp "github.com/coder/acp-go-sdk"
)

type server struct {
	cfg     config
	mgr     *manager
	respond *respondClient
}

func newServer(cfg config) *server {
	return &server{
		cfg:     cfg,
		mgr:     newManager(cfg),
		respond: newRespondClient(cfg.respondBaseURL, cfg.respondToken),
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", s.handleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// handleWebhook acks immediately and processes the event asynchronously:
// respond.io retries slow deliveries, and a prompt turn far outlives the
// webhook timeout.
func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var ev webhookEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)

	switch ev.EventType {
	case "message.received":
		go s.handleIncoming(ev)
	case "message.sent":
		if s.cfg.outgoingCommand != "" {
			go s.handleOutgoing(ev)
		}
	default:
		log.Printf("ignoring event %q", ev.EventType)
	}
}

func (s *server) handleIncoming(ev webhookEvent) {
	blocks, err := s.contentBlocks(ev)
	if err != nil {
		log.Printf("contact %d: %v", ev.Contact.ID, err)
		return
	}
	if blocks == nil {
		log.Printf("contact %d: dropping unsupported message type %q", ev.Contact.ID, ev.Message.Message.Type)
		return
	}
	deliver := func(text string) error {
		return s.respond.sendText(ev.Contact.ID, text)
	}
	if err := s.mgr.prompt(context.Background(), ev.Contact.ID, blocks, deliver); err != nil {
		log.Printf("contact %d: %v", ev.Contact.ID, err)
	}
}

// handleOutgoing records messages sent to the contact by others (agents,
// workflows) into the harness context via the configured slash command.
// Messages this server sent are skipped to avoid echo loops.
func (s *server) handleOutgoing(ev webhookEvent) {
	if s.respond.wasSentByUs(ev.Message.MessageID) {
		return
	}
	if ev.Message.Message.Type != "text" || ev.Message.Message.Text == "" {
		return
	}
	prompt := s.cfg.outgoingCommand + "\n" + ev.Message.Message.Text
	err := s.mgr.prompt(context.Background(), ev.Contact.ID, []acp.ContentBlock{acp.TextBlock(prompt)}, nil)
	if err != nil {
		log.Printf("contact %d: outgoing: %v", ev.Contact.ID, err)
	}
}

// contentBlocks translates a respond.io message into ACP content blocks.
// Returns nil for unsupported types.
func (s *server) contentBlocks(ev webhookEvent) ([]acp.ContentBlock, error) {
	m := ev.Message.Message
	switch m.Type {
	case "text":
		if m.Text == "" {
			return nil, nil
		}
		return []acp.ContentBlock{acp.TextBlock(m.Text)}, nil
	case "attachment":
		if m.Attachment == nil {
			return nil, nil
		}
		return s.attachmentBlocks(ev.Contact.ID, *m.Attachment)
	default:
		return nil, nil
	}
}

func (s *server) attachmentBlocks(contactID int64, a attachment) ([]acp.ContentBlock, error) {
	data, err := s.respond.download(a.URL)
	if err != nil {
		return nil, fmt.Errorf("attachment %s: %w", a.URL, err)
	}
	switch a.Type {
	case "image":
		return []acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString(data), a.Mime)}, nil
	case "audio":
		return []acp.ContentBlock{acp.AudioBlock(base64.StdEncoding.EncodeToString(data), a.Mime)}, nil
	default:
		// Other files land in the user's cwd; the agent reads them with its
		// own fs tools via the resource link.
		name := filepath.Base(a.FileName)
		if name == "" || name == "." {
			name = "attachment"
		}
		dir := filepath.Join(s.cfg.dataDir, fmt.Sprint(contactID))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
		return []acp.ContentBlock{
			acp.TextBlock(fmt.Sprintf("The user sent a file: %s", name)),
			acp.ResourceLinkBlock(name, "file://"+path),
		}, nil
	}
}
