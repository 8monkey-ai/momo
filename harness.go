package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	acp "github.com/coder/acp-go-sdk"
)

// userSession owns one harness subprocess and one ACP session for a contact.
type userSession struct {
	cmd       *exec.Cmd
	waitOnce  sync.Once // cmd.Wait must run exactly once
	conn      *acp.ClientSideConnection
	sessionID acp.SessionId
	// turn routes streamed chunks: the actor stores it around each prompt,
	// the connection's read goroutine loads it.
	turn atomic.Pointer[turn]
}

// spawn starts the harness for a contact and creates (or loads) the ACP
// session.
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
