// Package sessionhistory adds a conversation part momo did not answer to the
// agent's session: a message a human operator or a workflow handled reaches the
// agent as a slash command, so the session keeps the whole conversation.
//
// The extension is optional. An operator enables it, and selects the commands the
// configured agent serves; momo cannot discover them.
package sessionhistory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/8monkey-ai/momo/internal/core"
)

// Role is the part of the conversation a recorded message belongs to.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Recorder puts one message into the agent's session. It answers with an error
// because the channel that recorded the message logs a failed record; nothing of
// it reaches the contact.
type Recorder interface {
	Record(ctx context.Context, m core.Message, role Role) error
}

// Prompter is what the extension needs of the harness: one prompt of one
// conversation, serialised with the turns of that conversation.
type Prompter interface {
	Prompt(ctx context.Context, conversation string, prompt []core.ContentBlock, emit core.Emit) error
}

type settings struct {
	RecordUserMessageCommand      string `yaml:"record_user_message_command"`
	RecordAssistantMessageCommand string `yaml:"record_assistant_message_command"`
}

// New reads the extension's block and refuses an enabled extension momo cannot
// record with, before the process serves anything.
func New(log *slog.Logger, decode func(any) error, p Prompter) (Recorder, error) {
	var s settings
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.RecordUserMessageCommand == "" || s.RecordAssistantMessageCommand == "" {
		return nil, errors.New("record_user_message_command and record_assistant_message_command are required")
	}
	return recorder{log: log, prompter: p, commands: map[Role]string{
		RoleUser:      s.RecordUserMessageCommand,
		RoleAssistant: s.RecordAssistantMessageCommand,
	}}, nil
}

type recorder struct {
	log      *slog.Logger
	prompter Prompter
	commands map[Role]string
}

// Record sends the role's command with the message's text as its argument. Every
// line break becomes one space, because a slash command ends at the end of the
// line.
func (r recorder) Record(ctx context.Context, m core.Message, role Role) error {
	command, known := r.commands[role]
	if !known {
		return fmt.Errorf("no command records the role %q", role)
	}
	text := core.TextOf(m.Content)
	if text == "" {
		return errors.New("a message with no text cannot be recorded")
	}
	prompt := core.Text(command + " " + strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(text))
	return r.prompter.Prompt(ctx, m.Conversation, prompt, r.discard(m.Conversation))
}

// discard drops what the agent streams for a record: nobody is waiting for it.
//
// ponytail: ACP carries a slash command in an ordinary prompt, so an agent that
// does not know the command answers it like any other prompt, and momo cannot tell
// the two apart. This debug line is the only diagnosis this path offers.
func (r recorder) discard(conversation string) core.Emit {
	return func(content []core.ContentBlock) error {
		r.log.Debug("session history reply discarded", "conversation", conversation, "text", core.TextOf(content))
		return nil
	}
}

// Qualified prefixes a recorded conversation with the channel's configured name,
// the way the handler's messages are qualified, so one contact on one channel is
// one conversation for a record and for a turn alike. An absent recorder stays
// absent, so a channel's own nil check holds.
func Qualified(name string, r Recorder) Recorder {
	if r == nil {
		return nil
	}
	return qualifying{name: name, recorder: r}
}

type qualifying struct {
	name     string
	recorder Recorder
}

func (q qualifying) Record(ctx context.Context, m core.Message, role Role) error {
	m.Conversation = q.name + ":" + m.Conversation
	return q.recorder.Record(ctx, m, role)
}
