// Command testagent is a minimal ACP agent used by the agent-server tests.
// It replies to every prompt with two paragraphs echoing the input, streamed
// as separate agent_message_chunk updates. Prompts starting with a slash
// command mimic pi-acp's record-only turns: one ack chunk, then the prompt
// never resolves.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

type agent struct {
	conn *acp.AgentSideConnection
}

var _ acp.Agent = (*agent)(nil)

func (a *agent) Initialize(ctx context.Context, p acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
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
	return acp.NewSessionResponse{SessionId: "test-session"}, nil
}

func (a *agent) CloseSession(ctx context.Context, p acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}

func (a *agent) ListSessions(ctx context.Context, p acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
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
			return acp.PromptResponse{}, err
		}
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func main() {
	a := &agent{}
	a.conn = acp.NewAgentSideConnection(a, os.Stdout, os.Stdin)
	<-a.conn.Done()
}
