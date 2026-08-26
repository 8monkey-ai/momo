// Package sessionhistorysync keeps the agent's session in step with a
// conversation a human answered: the contact's message and the human's reply
// reach the session as prompt turns of the agent's own slash commands, so the
// agent's next turn reads what happened while it was not answering.
package sessionhistorysync

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/8monkey-ai/momo/internal/core"
)

type settings struct {
	UserMessageCommand      string `yaml:"user_message_command"`
	AssistantMessageCommand string `yaml:"assistant_message_command"`
}

type recorder struct {
	log       *slog.Logger
	agent     core.Agent
	user      string
	assistant string
}

// New builds a recorder when session history sync is configured.
func New(log *slog.Logger, decode func(any) error, a core.Agent) (core.History, error) {
	if decode == nil {
		return nil, nil
	}
	var s settings
	if err := decode(&s); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(s.UserMessageCommand, "/") || !strings.HasPrefix(s.AssistantMessageCommand, "/") {
		return nil, errors.New("user_message_command and assistant_message_command are both required and must start with \"/\"")
	}
	return recorder{log: log, agent: a, user: s.UserMessageCommand, assistant: s.AssistantMessageCommand}, nil
}

func (r recorder) RecordUser(ctx context.Context, m core.Message) {
	r.record(ctx, m, r.user)
}

func (r recorder) RecordAssistant(ctx context.Context, m core.Message) {
	r.record(ctx, m, r.assistant)
}

// record sends one slash command as a prompt turn of the conversation. The turn
// is the same seam a message's turn uses, so a record never runs beside the turn
// of a message of that conversation.
func (r recorder) record(ctx context.Context, m core.Message, command string) {
	prompt := core.Message{
		Conversation: m.Conversation,
		Content:      core.Text(command + " " + lineBreaks.Replace(core.TextOf(m.Content))),
	}
	// Nobody is waiting for an answer to a record, so what the agent says is read
	// for diagnosis only: an agent that does not support the command answers with
	// its own text, and momo verifies no command.
	var answer []string
	answered := func(content []core.ContentBlock) error {
		answer = append(answer, core.TextOf(content))
		return nil
	}
	if err := r.agent.Turn(ctx, prompt, answered); err != nil {
		r.log.Error("history record failed", "conversation", m.Conversation, "command", command, "error", err)
		return
	}
	if text := strings.Join(answer, " "); text != "" {
		r.log.Info("the agent answered a history record", "conversation", m.Conversation, "command", command, "answer", text)
	}
}

// lineBreaks holds a slash command on one line: a command reaches the agent as
// one prompt, and a line break in it would end the command early.
var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")
