// Package sessionhistory writes a message momo did not produce into the agent's
// session, so the session keeps the whole conversation while a human answers it.
package sessionhistory

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/8monkey-ai/momo/internal/core"
)

// Name is the name operators configure this extension under.
const Name = "session-history-sync"

type settings struct {
	UserMessageCommand      string `yaml:"user_message_command"`
	AssistantMessageCommand string `yaml:"assistant_message_command"`
}

// Sync records a message as a slash command prompt on the agent that runs the
// conversation's turns. The agent owns the commands; momo neither discovers nor
// verifies them.
type Sync struct {
	log       *slog.Logger
	user      string
	assistant string
	agent     core.Agent
}

// New reads the extension's block and refuses a configuration that cannot
// record: without both commands, a recorded message would reach the session as
// ordinary text the agent answers.
func New(log *slog.Logger, decode func(any) error, a core.Agent) (*Sync, error) {
	var s settings
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.UserMessageCommand == "" || s.AssistantMessageCommand == "" {
		return nil, errors.New("user_message_command and assistant_message_command are required")
	}
	return &Sync{log: log, user: s.UserMessageCommand, assistant: s.AssistantMessageCommand, agent: a}, nil
}

func (s *Sync) RecordUser(ctx context.Context, m core.Message) {
	s.record(ctx, s.user, m)
}

func (s *Sync) RecordAssistant(ctx context.Context, m core.Message) {
	s.record(ctx, s.assistant, m)
}

// lineBreaks keeps the command on one line: a slash command ends at the first
// line break, so a message written in three lines would record its first line
// only.
var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// record runs the command as an ordinary prompt turn, so it takes its place in
// the order the harness already keeps for that conversation. What the agent
// answered stays in the log, so nothing of it reaches the contact.
func (s *Sync) record(ctx context.Context, command string, m core.Message) {
	prompt := core.Message{
		Conversation: m.Conversation,
		Content:      core.Text(command + " " + lineBreaks.Replace(core.TextOf(m.Content))),
	}
	var answer strings.Builder
	err := s.agent.Turn(ctx, prompt, func(content []core.ContentBlock) error {
		answer.WriteString(core.TextOf(content))
		return nil
	})
	if err != nil {
		s.log.Error("history record failed", "conversation", m.Conversation, "command", command, "error", err)
	}
	// An agent that does not support the command refuses in its reply, which is the
	// only place the operator can read it.
	if answer.Len() > 0 {
		s.log.Info("history record answered",
			"conversation", m.Conversation, "command", command, "answer", answer.String())
	}
}
