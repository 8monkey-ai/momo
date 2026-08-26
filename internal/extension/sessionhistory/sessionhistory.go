// Package sessionhistory keeps an agent session current while somebody else
// answers the contact: each message a human writes, in either direction, is
// recorded in the session with a slash command the agent serves.
package sessionhistory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/8monkey-ai/momo/internal/core"
)

type settings struct {
	UserCommand      string `yaml:"user_command"`
	AssistantCommand string `yaml:"assistant_command"`
}

// New reads the session_history block. Without the block momo records nothing,
// which is what a deployment with no human handover needs.
func New(log *slog.Logger, decode func(any) error, a core.Agent) (core.Recorder, error) {
	var s settings
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.UserCommand == "" && s.AssistantCommand == "" {
		return off{}, nil
	}
	if s.UserCommand == "" || s.AssistantCommand == "" {
		return nil, errors.New("user_command and assistant_command are both required")
	}
	if s.UserCommand == s.AssistantCommand {
		return nil, errors.New("user_command and assistant_command must be two different commands")
	}
	for _, command := range []string{s.UserCommand, s.AssistantCommand} {
		if err := usable(command); err != nil {
			return nil, err
		}
	}
	return recorder{log: log, agent: a, user: s.UserCommand, assistant: s.AssistantCommand}, nil
}

// usable refuses what an agent cannot read as one slash command with the
// message behind it.
func usable(command string) error {
	if !strings.HasPrefix(command, "/") {
		return fmt.Errorf("command %q must begin with \"/\"", command)
	}
	if strings.ContainsAny(command, " \t\r\n") {
		return fmt.Errorf("command %q must be one word", command)
	}
	return nil
}

type off struct{}

func (off) Enabled() bool                                       { return false }
func (off) RecordUser(context.Context, core.Message) error      { return nil }
func (off) RecordAssistant(context.Context, core.Message) error { return nil }

type recorder struct {
	log       *slog.Logger
	agent     core.Agent
	user      string
	assistant string
}

func (recorder) Enabled() bool { return true }

func (r recorder) RecordUser(ctx context.Context, m core.Message) error {
	return r.record(ctx, m, r.user)
}

func (r recorder) RecordAssistant(ctx context.Context, m core.Message) error {
	return r.record(ctx, m, r.assistant)
}

// record runs one turn that only writes to the session. The agent's answer goes
// to the log, because nobody is waiting for it.
func (r recorder) record(ctx context.Context, m core.Message, command string) error {
	text := oneLine(core.TextOf(m.Content))
	if text == "" {
		return nil
	}
	prompt := core.Message{Conversation: m.Conversation, Content: core.Text(command + " " + text)}
	var answer strings.Builder
	if err := r.agent.Turn(ctx, prompt, func(content []core.ContentBlock) error {
		answer.WriteString(core.TextOf(content))
		return nil
	}); err != nil {
		r.log.Error("history record failed", "conversation", m.Conversation, "command", command, "error", err)
		return fmt.Errorf("recording with %s: %w", command, err)
	}
	// An agent that does not serve the command answers with text instead of
	// silence, and that text is the only sign momo gets of it.
	if said := strings.TrimSpace(answer.String()); said != "" {
		r.log.Warn("history record answered", "conversation", m.Conversation, "command", command, "answer", said)
	}
	return nil
}

// oneLine keeps the record on the single line a slash command occupies.
func oneLine(text string) string {
	replaced := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
	return strings.TrimSpace(replaced)
}
