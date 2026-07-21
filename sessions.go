package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	acp "github.com/coder/acp-go-sdk"
)

// userSession owns one harness subprocess and one ACP session for a contact.
type userSession struct {
	mu          sync.Mutex // serializes prompt turns
	cmd         *exec.Cmd
	waitOnce    sync.Once // cmd.Wait must run exactly once
	conn        *acp.ClientSideConnection
	sessionID   acp.SessionId
	currentTurn *turn      // non-nil while a prompt turn is streaming
	turnMu      sync.Mutex // guards currentTurn
}

// manager maps respond.io contacts to live harness sessions.
type manager struct {
	cfg config

	mu    sync.Mutex
	users map[int64]*userSession
}

func newManager(cfg config) *manager {
	return &manager{
		cfg:   cfg,
		users: make(map[int64]*userSession),
	}
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
	cwd, err := m.cfg.contactDir(contactID)
	if err != nil {
		return nil, err
	}
	if err := seedContactDir(cwd, m.cfg.contactTemplate); err != nil {
		return nil, err
	}

	cmd := exec.Command(m.cfg.agentCmd)
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

	s := &userSession{cmd: cmd}
	conn := acp.NewClientSideConnection(&acpClient{sess: s}, stdin, stdout)
	s.conn = conn

	init, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	})
	if err != nil {
		s.shutdown()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	if init.AgentCapabilities.LoadSession {
		if prev := latestSession(ctx, conn, cwd); prev != "" {
			_, err := conn.LoadSession(ctx, acp.LoadSessionRequest{
				SessionId:  prev,
				Cwd:        cwd,
				McpServers: []acp.McpServer{},
			})
			if err == nil {
				s.sessionID = prev
				m.watch(contactID, s)
				return s, nil
			}
			log.Printf("contact %d: session/load %q failed, starting fresh: %v", contactID, prev, err)
		}
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
	m.watch(contactID, s)
	return s, nil
}

// latestSession returns the most recently updated session the agent lists for
// cwd, or "" if it lists none or doesn't support session/list.
func latestSession(ctx context.Context, conn *acp.ClientSideConnection, cwd string) acp.SessionId {
	var newest acp.SessionInfo
	var cursor *string
	for {
		resp, err := conn.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd, Cursor: cursor})
		if err != nil {
			return ""
		}
		for _, sess := range resp.Sessions {
			if newest.UpdatedAt == nil ||
				(sess.UpdatedAt != nil && *sess.UpdatedAt > *newest.UpdatedAt) {
				newest = sess
			}
		}
		if resp.NextCursor == nil {
			return newest.SessionId
		}
		cursor = resp.NextCursor
	}
}

// seedContactDir symlinks the template's entries into the contact dir so the
// harness finds its project config (e.g. gato's .pi/, AGENTS.md) in the
// session cwd. Existing entries are left alone; .git is skipped.
func seedContactDir(cwd, template string) error {
	if template == "" {
		return nil
	}
	abs, err := filepath.Abs(template)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return fmt.Errorf("contact template: %w", err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		link := filepath.Join(cwd, e.Name())
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join(abs, e.Name()), link); err != nil {
			return err
		}
	}
	return nil
}

// watch drops the session from the registry when the harness exits.
func (m *manager) watch(contactID int64, s *userSession) {
	go func() {
		<-s.conn.Done()
		s.wait()
		m.mu.Lock()
		if m.users[contactID] == s {
			delete(m.users, contactID)
		}
		m.mu.Unlock()
		log.Printf("contact %d: harness exited", contactID)
	}()
}

// shutdown SIGTERMs pi-acp; its exit breaks the stdio pipe and pi shuts down
// gracefully on stdin EOF, persisting its session (verified empirically).
func (s *userSession) shutdown() {
	if s.cmd.Process != nil {
		s.cmd.Process.Signal(syscall.SIGTERM)
	}
	s.wait()
}

func (s *userSession) wait() {
	s.waitOnce.Do(func() { s.cmd.Wait() })
}

var errHarnessGone = errors.New("harness died mid-prompt")

// prompt runs one turn. If a turn is already streaming, it steers: cancels
// the active turn, then queues the new prompt (turns are serialized by mu).
// A prompt queued behind a harness recycle finds its session dead; retry once.
func (m *manager) prompt(ctx context.Context, contactID int64, blocks []acp.ContentBlock, deliver func(string) error) error {
	err := m.promptOnce(ctx, contactID, blocks, deliver)
	if errors.Is(err, errHarnessGone) {
		log.Printf("contact %d: harness died mid-prompt, retrying with a fresh one", contactID)
		err = m.promptOnce(ctx, contactID, blocks, deliver)
	}
	return err
}

func (m *manager) promptOnce(ctx context.Context, contactID int64, blocks []acp.ContentBlock, deliver func(string) error) error {
	s, err := m.sessionFor(ctx, contactID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	t := newTurn(deliver, m.cfg.typingPerWord)
	if deliver == nil {
		// The record command's prompt never resolves (no agent loop —
		// https://github.com/svkozak/pi-acp/issues/84); end the turn at the
		// ack chunk instead. FUTURE: drop once fixed upstream.
		t.onFirstChunk = cancel
	}
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
		// A cancelled ctx on a record-only turn means the ack fired: its
		// expected end, not a failure.
		if deliver == nil && ctx.Err() != nil {
			m.terminate(contactID, s)
			return nil
		}
		select {
		case <-s.conn.Done():
			m.terminate(contactID, s)
			return errHarnessGone
		default:
		}
		m.terminate(contactID, s)
		return fmt.Errorf("prompt: %w", err)
	}
	t.finish(resp.StopReason != acp.StopReasonCancelled)
	// A cancelled turn means a steering prompt is queued on s.mu; keep the
	// harness alive so it lands in the live session.
	if resp.StopReason == acp.StopReasonCancelled {
		return nil
	}
	// Terminate after every completed turn: recorded messages and restored
	// history only apply on the next session rebuild (session/load). FUTURE:
	// termination is server-driven only because pi-acp can't survive its pi
	// child dying (https://github.com/svkozak/pi-acp/issues/82); once fixed,
	// the harness could end its own session instead.
	m.terminate(contactID, s)
	return nil
}

// terminate shuts a harness down and deregisters it synchronously, so the
// next message respawns cleanly instead of racing watch()'s async cleanup.
func (m *manager) terminate(contactID int64, s *userSession) {
	s.shutdown()
	m.mu.Lock()
	if m.users[contactID] == s {
		delete(m.users, contactID)
	}
	m.mu.Unlock()
}

// stopAll shuts down every live harness; used on server shutdown.
func (m *manager) stopAll() {
	m.mu.Lock()
	sessions := make([]*userSession, 0, len(m.users))
	for _, s := range m.users {
		sessions = append(sessions, s)
	}
	m.users = make(map[int64]*userSession)
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Go(s.shutdown)
	}
	wg.Wait()
}
