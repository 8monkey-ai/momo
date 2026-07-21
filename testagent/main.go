// Command testagent is a minimal ACP agent for the agent-server tests. It
// mimics pi-acp where the tests depend on it: slash prompts are record-only
// turns (one ack chunk, never resolves), and sessions persist to a file in
// the cwd so session/list finds them across process recycles.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

const (
	sessionsFile = ".testagent-sessions.json"
	promptsLog   = ".testagent-prompts.log" // one line per prompt, in arrival order
	releaseFile  = "release"                // unblocks "block"-prefixed prompts
)

type agent struct {
	conn *acp.AgentSideConnection
	mu   sync.Mutex
}

var (
	_ acp.Agent       = (*agent)(nil)
	_ acp.AgentLoader = (*agent)(nil)
)

func (a *agent) Initialize(ctx context.Context, p acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession:         true,
			SessionCapabilities: acp.SessionCapabilities{List: &acp.SessionListCapabilities{}},
		},
	}, nil
}

func (a *agent) Authenticate(ctx context.Context, p acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *agent) Logout(ctx context.Context, p acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}

func (a *agent) Cancel(ctx context.Context, p acp.CancelNotification) error {
	return nil
}

func (a *agent) NewSession(ctx context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	id := acp.SessionId(fmt.Sprintf("test-session-%d", time.Now().UnixNano()))
	a.putSession(sessionRecord{
		SessionId: string(id),
		Cwd:       p.Cwd,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	return acp.NewSessionResponse{SessionId: id}, nil
}

func (a *agent) CloseSession(ctx context.Context, p acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func (a *agent) ListSessions(ctx context.Context, p acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	sessions := []acp.SessionInfo{}
	for _, r := range a.loadSessions() {
		if p.Cwd != nil && r.Cwd != *p.Cwd {
			continue
		}
		sessions = append(sessions, acp.SessionInfo{
			SessionId: acp.SessionId(r.SessionId),
			Cwd:       r.Cwd,
			UpdatedAt: &r.UpdatedAt,
		})
	}
	return acp.ListSessionsResponse{Sessions: sessions}, nil
}

func (a *agent) LoadSession(ctx context.Context, p acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	for _, r := range a.loadSessions() {
		if r.SessionId == string(p.SessionId) {
			return acp.LoadSessionResponse{}, nil
		}
	}
	return acp.LoadSessionResponse{}, fmt.Errorf("unknown session %q", p.SessionId)
}

func (a *agent) ResumeSession(ctx context.Context, p acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}

func (a *agent) SetSessionConfigOption(ctx context.Context, p acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}

func (a *agent) SetSessionMode(ctx context.Context, p acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

func (a *agent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	text := "?"
	for _, b := range p.Prompt {
		if b.Text != nil {
			text = b.Text.Text
		}
	}
	logPrompt(text)
	if strings.HasPrefix(text, "block:") {
		awaitFile(cwdPath(releaseFile))
	}
	if strings.HasPrefix(text, "/") {
		err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: p.SessionId,
			Update:    acp.UpdateAgentMessageText("Appended a message; it applies on the next session rebuild."),
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		select {} // never resolves, like pi-acp
	}
	if release, ok := strings.CutPrefix(text, "hold:"); ok {
		// Stream one paragraph so the client sees the turn running, then stay
		// blocked past session/cancel until the release file appears.
		err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: p.SessionId,
			Update:    acp.UpdateAgentMessageText("holding\n\n"),
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		<-ctx.Done()
		awaitFile(release)
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	if strings.HasPrefix(text, "cancelme:") {
		// Stream a partial chunk (no paragraph boundary), then wait for the
		// steer's session/cancel; the reply never completes.
		err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: p.SessionId,
			Update:    acp.UpdateAgentMessageText("partial reply to " + text),
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		<-ctx.Done()
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	chunks := []string{
		fmt.Sprintf("You said: %s", text),
		"\n\nSecond paragraph.",
	}
	for _, c := range chunks {
		err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: p.SessionId,
			Update:    acp.UpdateAgentMessageText(c),
		})
		if err != nil {
			// session/cancel cancels our handler ctx; report a cancelled
			// turn like a real agent would.
			if ctx.Err() != nil {
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
			}
			return acp.PromptResponse{}, err
		}
	}
	if ctx.Err() != nil {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

type sessionRecord struct {
	SessionId string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	UpdatedAt string `json:"updatedAt"`
}

// cwdPath resolves name in the process cwd — the server spawns us with the
// session cwd as process cwd.
func cwdPath(name string) string {
	wd, _ := os.Getwd()
	return filepath.Join(wd, name)
}

func sessionsPath() string {
	return cwdPath(sessionsFile)
}

// logPrompt appends each prompt in arrival order so tests can assert which
// prompts reached the agent.
func logPrompt(text string) {
	f, err := os.OpenFile(cwdPath(promptsLog), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, text)
}

// awaitFile blocks until path exists.
func awaitFile(path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readSessions() []sessionRecord {
	var recs []sessionRecord
	if b, err := os.ReadFile(sessionsPath()); err == nil {
		json.Unmarshal(b, &recs)
	}
	return recs
}

func (a *agent) loadSessions() []sessionRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return readSessions()
}

func (a *agent) putSession(rec sessionRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, _ := json.Marshal(append(readSessions(), rec))
	os.WriteFile(sessionsPath(), b, 0o644)
}

func main() {
	a := &agent{}
	a.conn = acp.NewAgentSideConnection(a, os.Stdout, os.Stdin)
	<-a.conn.Done()
}
