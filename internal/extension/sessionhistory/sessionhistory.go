// Package sessionhistory keeps an agent session in step with a conversation a
// human operator answers. A message momo does not reply to is recorded in the
// session with a slash command the agent serves, so the history the agent reads
// holds what the contact and the operator wrote while momo stayed quiet.
//
// The extension is optional: a deployment without human handover runs without it.
// Which message is recorded, and which is answered, is the channel's decision;
// this package owns the command a record is written with and nothing else.
package sessionhistory

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/8monkey-ai/momo/internal/core"
)

type settings struct {
	UserCommand      string `yaml:"user_command"`
	AssistantCommand string `yaml:"assistant_command"`
}

// New builds the recorder from the session_history block. A nil decode is a
// configuration without the block, and the answer is then no recorder: a channel
// reads that as the extension being absent.
func New(log *slog.Logger, decode func(any) error, a core.Agent) (core.Handler, error) {
	if decode == nil {
		return nil, nil
	}
	var s settings
	if err := decode(&s); err != nil {
		return nil, err
	}
	// One command without the other records one half of the conversation, which
	// leaves the session further from the truth than no record at all.
	if s.UserCommand == "" || s.AssistantCommand == "" {
		return nil, errors.New("user_command and assistant_command are required")
	}
	return history{log: log, agent: a, user: s.UserCommand, assistant: s.AssistantCommand}, nil
}

type history struct {
	log       *slog.Logger
	agent     core.Agent
	user      string
	assistant string
}

// Received records what the contact wrote. Nothing answers it, and a failed
// record is not the channel's to report: the conversation belongs to a human.
func (h history) Received(ctx context.Context, m core.Message, _ core.Reply) error {
	h.record(ctx, m, h.user)
	return nil
}

func (h history) Sent(ctx context.Context, m core.Message) {
	h.record(ctx, m, h.assistant)
}

// oneLine is every line break a command must not carry: an agent reads a prompt's
// second line as text and not as part of the command.
var oneLine = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")

// record writes one message into the session on the agent's own prompt path, so it
// waits for the same seam every other turn of the conversation waits for.
func (h history) record(ctx context.Context, m core.Message, command string) {
	text := strings.TrimSpace(oneLine.Replace(core.TextOf(m.Content)))
	if text == "" {
		return
	}
	prompt := core.Message{Conversation: m.Conversation, Content: core.Text(command + " " + text)}
	// A record asks for nothing, so whatever the agent answers is a diagnosis: an
	// agent that does not serve the configured command says so here.
	output := func(content []core.ContentBlock) error {
		h.log.Warn("the agent answered a session history command", "conversation", m.Conversation, "output", core.TextOf(content))
		return nil
	}
	if err := h.agent.Turn(ctx, prompt, output); err != nil {
		h.log.Error("message not recorded in the session history", "conversation", m.Conversation, "error", err)
	}
}
