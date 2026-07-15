package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// Slash commands from gato's @8monkey/pi-context-history package: they append
// to the chat history without triggering a generation.
const (
	recordIncomingCommand = "/add-user-message"
	recordOutgoingCommand = "/add-assistant-message"
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
		respond: newRespondClient(cfg.apiBaseURL, cfg.apiToken),
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	var ev webhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	var key string
	var handle func(webhookEvent)
	switch ev.EventType {
	case "message.received":
		key, handle = s.cfg.incomingSigningKey, s.handleIncoming
	case "message.sent":
		key, handle = s.cfg.outgoingSigningKey, s.handleOutgoing
	}
	if key != "" && !validSignature(body, r.Header.Get("X-Webhook-Signature"), key) {
		log.Printf("rejected %q webhook: invalid signature", ev.EventType)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)

	if handle == nil {
		log.Printf("ignoring event %q", ev.EventType)
		return
	}
	go handle(ev)
}

// handleIncoming prompts the harness for a reply, unless a human assignee
// owns the conversation — then the message is only recorded into the context.
func (s *server) handleIncoming(ev webhookEvent) {
	if s.assignedToHuman(ev.Contact) {
		s.record(ev, recordIncomingCommand)
		return
	}
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

// handleOutgoing records messages sent to the contact by others (human
// operators, workflows) into the harness context. Our own replies are skipped
// to avoid echo loops; the AI-assignee check covers our sends even when the
// in-memory echo filter lost track (e.g. across server restarts).
func (s *server) handleOutgoing(ev webhookEvent) {
	if s.assignedToAI(ev.Contact) || s.respond.wasSentByUs(ev.Message.MessageID) {
		return
	}
	s.record(ev, recordOutgoingCommand)
}

func (s *server) assignedToHuman(c contact) bool {
	return s.cfg.aiAssigneeID != 0 && c.Assignee != nil && c.Assignee.ID != s.cfg.aiAssigneeID
}

func (s *server) assignedToAI(c contact) bool {
	return s.cfg.aiAssigneeID != 0 && c.Assignee != nil && c.Assignee.ID == s.cfg.aiAssigneeID
}

// record appends the message to the harness context without generating a
// reply. Non-text messages are dropped: they can't ride a slash command.
func (s *server) record(ev webhookEvent, command string) {
	if ev.Message.Message.Type != "text" || ev.Message.Message.Text == "" {
		return
	}
	// Single line: ACP adapters split the slash command from its args at the
	// first space, so the message must follow on the same line.
	prompt := command + " " + strings.ReplaceAll(ev.Message.Message.Text, "\n", " ")
	err := s.mgr.prompt(context.Background(), ev.Contact.ID, []acp.ContentBlock{acp.TextBlock(prompt)}, nil)
	if err != nil {
		log.Printf("contact %d: record %s: %v", ev.Contact.ID, command, err)
	}
}

// contentBlocks returns nil for unsupported message types.
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
