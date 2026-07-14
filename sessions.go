package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// userSession owns one harness subprocess and one ACP session for a contact.
type userSession struct {
	contactID int64

	mu          sync.Mutex // serializes prompt turns
	cmd         *exec.Cmd
	conn        *acp.ClientSideConnection
	sessionID   acp.SessionId
	currentTurn *turn      // non-nil while a prompt turn is streaming
	turnMu      sync.Mutex // guards currentTurn
}

// manager maps respond.io contacts to live harness sessions.
type manager struct {
	cfg   config
	store *sessionStore

	mu    sync.Mutex
	users map[int64]*userSession
}

func newManager(cfg config) *manager {
	return &manager{
		cfg:   cfg,
		store: newSessionStore(filepath.Join(cfg.dataDir, "sessions.json")),
		users: make(map[int64]*userSession),
	}
}

// acpClient handles agent→client calls. The harness uses its own built-in
// fs/terminal tools (we advertise no client capabilities), so only permission
// requests and session updates matter.
type acpClient struct {
	sess *userSession
}

var _ acp.Client = (*acpClient)(nil)

func (c *acpClient) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	for _, opt := range p.Options {
		if opt.Kind == acp.PermissionOptionKindAllowAlways {
			return permissionSelected(opt.OptionId), nil
		}
	}
	for _, opt := range p.Options {
		if opt.Kind == acp.PermissionOptionKindAllowOnce {
			return permissionSelected(opt.OptionId), nil
		}
	}
	if len(p.Options) > 0 {
		return permissionSelected(p.Options[0].OptionId), nil
	}
	return acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}},
	}, nil
}

func permissionSelected(id acp.PermissionOptionId) acp.RequestPermissionResponse {
	return acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{OptionId: id},
		},
	}
}

func (c *acpClient) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	u := n.Update
	if u.AgentMessageChunk == nil || u.AgentMessageChunk.Content.Text == nil {
		return nil
	}
	c.sess.turnMu.Lock()
	t := c.sess.currentTurn
	c.sess.turnMu.Unlock()
	if t != nil {
		t.addChunk(u.AgentMessageChunk.Content.Text.Text)
	}
	return nil
}

func (c *acpClient) ReadTextFile(ctx context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}

func (c *acpClient) WriteTextFile(ctx context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}

func (c *acpClient) CreateTerminal(ctx context.Context, p acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}

func (c *acpClient) KillTerminal(ctx context.Context, p acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}

func (c *acpClient) TerminalOutput(ctx context.Context, p acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}

func (c *acpClient) ReleaseTerminal(ctx context.Context, p acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}

func (c *acpClient) WaitForTerminalExit(ctx context.Context, p acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}

// sessionFor returns the live session for a contact, spawning the harness and
// creating (or loading) the ACP session on first use.
func (m *manager) sessionFor(ctx context.Context, contactID int64) (*userSession, error) {
	m.mu.Lock()
	if s, ok := m.users[contactID]; ok {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	s, err := m.spawn(ctx, contactID)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.users[contactID]; ok {
		s.shutdown()
		return existing, nil
	}
	m.users[contactID] = s
	return s, nil
}

func (m *manager) spawn(ctx context.Context, contactID int64) (*userSession, error) {
	cwd := filepath.Join(m.cfg.dataDir, fmt.Sprint(contactID))
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, err
	}

	cmd := exec.Command(m.cfg.agentCmd[0], m.cfg.agentCmd[1:]...)
	cmd.Dir = cwd
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting harness: %w", err)
	}

	s := &userSession{contactID: contactID, cmd: cmd}
	conn := acp.NewClientSideConnection(&acpClient{sess: s}, stdin, stdout)
	s.conn = conn

	init, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	})
	if err != nil {
		s.shutdown()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	if prev := m.store.get(contactID); prev != "" && init.AgentCapabilities.LoadSession {
		_, err := conn.LoadSession(ctx, acp.LoadSessionRequest{
			SessionId:  acp.SessionId(prev),
			Cwd:        cwd,
			McpServers: []acp.McpServer{},
		})
		if err == nil {
			s.sessionID = acp.SessionId(prev)
			m.watch(contactID, s)
			return s, nil
		}
		log.Printf("contact %d: session/load %q failed, starting fresh: %v", contactID, prev, err)
	}

	resp, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		s.shutdown()
		return nil, fmt.Errorf("session/new: %w", err)
	}
	s.sessionID = resp.SessionId
	m.store.put(contactID, string(resp.SessionId))
	m.watch(contactID, s)
	return s, nil
}

// watch drops the session from the registry when the harness exits.
func (m *manager) watch(contactID int64, s *userSession) {
	go func() {
		<-s.conn.Done()
		s.cmd.Wait()
		m.mu.Lock()
		if m.users[contactID] == s {
			delete(m.users, contactID)
		}
		m.mu.Unlock()
		log.Printf("contact %d: harness exited", contactID)
	}()
}

func (s *userSession) shutdown() {
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.cmd.Wait()
}

// prompt runs one turn. If a turn is already streaming, it steers: cancels the
// active turn, then queues the new prompt (prompt turns are serialized by mu).
func (m *manager) prompt(ctx context.Context, contactID int64, blocks []acp.ContentBlock, deliver func(string) error) error {
	s, err := m.sessionFor(ctx, contactID)
	if err != nil {
		return err
	}

	s.turnMu.Lock()
	steering := s.currentTurn != nil
	s.turnMu.Unlock()
	if steering {
		if err := s.conn.Cancel(ctx, acp.CancelNotification{SessionId: s.sessionID}); err != nil {
			log.Printf("contact %d: cancel: %v", contactID, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	t := newTurn(deliver, m.cfg.typingPerChar)
	s.turnMu.Lock()
	s.currentTurn = t
	s.turnMu.Unlock()

	resp, err := s.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: s.sessionID,
		Prompt:    blocks,
	})

	s.turnMu.Lock()
	s.currentTurn = nil
	s.turnMu.Unlock()

	if err != nil {
		t.finish(false)
		return fmt.Errorf("prompt: %w", err)
	}
	t.finish(resp.StopReason != acp.StopReasonCancelled)
	return nil
}

// sessionStore persists contactID→sessionID so harnesses that support
// session/load can resume conversations across agent-server restarts.
type sessionStore struct {
	path string
	mu   sync.Mutex
	m    map[string]string
}

func newSessionStore(path string) *sessionStore {
	s := &sessionStore{path: path, m: make(map[string]string)}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s.m); err != nil {
			log.Printf("session store %s corrupt, ignoring: %v", path, err)
		}
	}
	return s
}

func (s *sessionStore) get(contactID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[fmt.Sprint(contactID)]
}

func (s *sessionStore) put(contactID int64, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[fmt.Sprint(contactID)] = sessionID
	b, _ := json.Marshal(s.m)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		os.Rename(tmp, s.path)
	}
}
