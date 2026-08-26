// Package sessionhistory keeps an agent session complete while a human answers a
// conversation. What the contact and the human write is recorded in the session
// with a slash command the agent serves, so the agent knows what happened when it
// answers again.
package sessionhistory

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/core"
)

// oneLine puts a message on one line. An ACP agent reads a slash command and its
// argument on one line, so a line break would cut the argument off.
var oneLine = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")

type settings struct {
	UserMessageCommand      string `yaml:"user_message_command"`
	AssistantMessageCommand string `yaml:"assistant_message_command"`
}

// Sync is the pair of commands the agent records a message with.
type Sync struct {
	log       *slog.Logger
	user      string
	assistant string
}

// New reads the session_history_sync block. It answers nothing when the block is
// absent: a deployment without a human handover records nothing.
func New(log *slog.Logger, decode func(any) error) (*Sync, error) {
	var s settings
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.UserMessageCommand == "" && s.AssistantMessageCommand == "" {
		return nil, nil
	}
	if s.UserMessageCommand == "" || s.AssistantMessageCommand == "" {
		return nil, errors.New("user_message_command and assistant_message_command are both required")
	}
	return &Sync{log: log, user: s.UserMessageCommand, assistant: s.AssistantMessageCommand}, nil
}

// Recorder answers the recorder of one channel's conversations. h is the handler
// that channel's messages reach, so a record runs on the path a turn runs on and
// the two of one conversation never run at the same time.
func (s *Sync) Recorder(h core.Handler) channel.Recorder {
	return recorder{sync: s, handler: h}
}

type recorder struct {
	sync    *Sync
	handler core.Handler
}

func (r recorder) RecordUser(ctx context.Context, m core.Message) {
	r.record(ctx, r.sync.user, m)
}

func (r recorder) RecordAssistant(ctx context.Context, m core.Message) {
	r.record(ctx, r.sync.assistant, m)
}

// record prompts the agent with the command and the message behind it. Text only:
// a message that carries none is nothing a slash command can hold.
func (r recorder) record(ctx context.Context, command string, m core.Message) {
	text := oneLine.Replace(core.TextOf(m.Content))
	if strings.TrimSpace(text) == "" {
		return
	}
	prompt := core.Message{Conversation: m.Conversation, Content: core.Text(command + " " + text)}
	err := r.handler.Received(ctx, prompt, r.answered(m.Conversation, command))
	if err != nil {
		r.sync.log.Error("message not recorded", "conversation", m.Conversation, "command", command, "error", err)
	}
}

// answered logs what the agent replies to a record. An agent that does not serve
// the command answers with text and not with an error, so this log is the only
// place the operator reads that the pair of commands is wrong. The contact reads
// none of it.
func (r recorder) answered(conversation, command string) core.Reply {
	return func(_ context.Context, content []core.ContentBlock) error {
		if text := core.TextOf(content); text != "" {
			r.sync.log.Info("the agent answered a record", "conversation", conversation, "command", command, "answer", text)
		}
		return nil
	}
}
