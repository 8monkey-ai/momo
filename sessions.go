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
	if err := seedContactDir(cwd, m.cfg.contactTemplate); err != nil {
		return nil, err
	}

	cmd := exec.Command(m.cfg.agentCmd)
	cmd.Dir = cwd
	cmd.Stderr = os.Stderr
	// Own process group so shutdown() can SIGTERM pi-acp's pi child directly
	// instead of relying on pi-acp to forward the signal.
	// FUTURE: a plain SIGTERM to pi-acp alone almost suffices — pi-acp exits,
	// the broken stdio pipe makes pi shut down gracefully via its stdin-EOF
	// handler — but only if pi-acp actually exits; pi-acp's own child disposal
	// on shutdown is dead code (agent?.agent?.dispose?.() resolves undefined),
	// and a wedged pi-acp would leave pi idling forever. Once upstream disposes
	// its child reliably, drop the group signaling.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

// shutdown gracefully stops the harness by SIGTERM-ing its whole process
// group. pi's rpc-mode SIGTERM handler runs its shutdown (persisting the
// session and firing session_shutdown hooks), and pi-acp disposes its child
// and exits; the group signal reaches pi even if pi-acp is wedged.
func (s *userSession) shutdown() {
	if s.cmd.Process != nil {
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	}
	s.wait()
}

func (s *userSession) wait() {
	s.waitOnce.Do(func() { s.cmd.Wait() })
}

var errHarnessGone = errors.New("harness died mid-prompt")

// prompt runs one turn. If a turn is already streaming, it steers: cancels the
// active turn, then queues the new prompt (prompt turns are serialized by mu).
// A prompt that was queued behind a harness recycle finds its session dead;
// retry once with a freshly spawned harness.
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
		// The record command acks with a chunk once the message is persisted;
		// the prompt itself never resolves (pi runs extension commands without
		// an agent loop, so pi-acp never sees agent_end — upstream bug). End
		// the turn at the ack.
		// FUTURE: once https://github.com/svkozak/pi-acp/issues/84 is fixed
		// (command-only prompts resolve their turn), drop this ack wait and
		// let the Prompt call resolve normally.
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
		// A record-only turn never resolves cleanly (no agent loop), so a
		// cancelled ctx here is its expected end (the ack fired), not a failure.
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
	// A cancelled turn means a steering prompt is already queued on s.mu; keep
	// the harness alive so it lands in the live session instead of paying a
	// respawn. The batch's final (uncancelled) turn terminates as usual.
	if resp.StopReason == acp.StopReasonCancelled {
		return nil
	}
	// Terminate after every completed turn: recorded messages only apply on
	// the next session rebuild, and a fresh session/load restores them.
	// FUTURE: server-driven termination exists because pi-acp neither notices a
	// dead pi child nor recovers from it (session/prompt silently resolves as an
	// empty end_turn — https://github.com/svkozak/pi-acp/issues/82). Once fixed
	// upstream, revisit: the harness could end its own session after each
	// generation (e.g. a project extension on agent_settled), making this a
	// bare session/load-before-prompt with no kill.
	m.terminate(contactID, s)
	return nil
}

// terminate gracefully shuts a harness down and deregisters it, so the next
// message respawns cleanly instead of racing watch()'s async cleanup. Only the
// still-registered pointer is removed, matching watch().
func (m *manager) terminate(contactID int64, s *userSession) {
	s.shutdown()
	m.mu.Lock()
	if m.users[contactID] == s {
		delete(m.users, contactID)
	}
	m.mu.Unlock()
}

// stopAll gracefully shuts down every live harness. Used on server shutdown.
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
