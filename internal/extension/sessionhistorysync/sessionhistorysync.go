// Package sessionhistorysync keeps an agent session's history complete while
// someone other than momo answers a conversation: a message momo did not
// generate is sent to the agent as a slash command, so the next turn sees the
// whole conversation.
//
// The synchronization is optional. An operator enables it by configuring the
// commands their agent serves, and a channel that gets no recorder keeps its
// plain behavior.
package sessionhistorysync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/8monkey-ai/momo/internal/core"
)

// Role is who a recorded message came from, in the agent session's terms.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Recorder adds a message to a conversation's agent session without answering
// it. The error is for the channel's log only: a record nobody asked for has
// nothing to report to the contact.
type Recorder interface {
	Record(ctx context.Context, m core.Message, role Role) error
}

// Prompter is the one thing this extension needs of an agent harness: a prompt
// on a conversation, serialized with that conversation's turns.
type Prompter interface {
	Prompt(ctx context.Context, conversation string, prompt []core.ContentBlock, emit core.Emit) error
}

type settings struct {
	RecordUserMessageCommand      string `yaml:"record_user_message_command"`
	RecordAssistantMessageCommand string `yaml:"record_assistant_message_command"`
}

// New reads the extension's block and refuses a configuration it cannot record
// with, before the process serves anything. momo ships no command defaults:
// which slash commands exist is the configured agent's decision.
func New(log *slog.Logger, decode func(any) error, p Prompter) (Recorder, error) {
	var s settings
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.RecordUserMessageCommand == "" {
		return nil, errors.New("record_user_message_command is required")
	}
	if s.RecordAssistantMessageCommand == "" {
		return nil, errors.New("record_assistant_message_command is required")
	}
	return recorder{log: log, commands: map[Role]string{
		RoleUser:      s.RecordUserMessageCommand,
		RoleAssistant: s.RecordAssistantMessageCommand,
	}, prompter: p}, nil
}

type recorder struct {
	log      *slog.Logger
	commands map[Role]string
	prompter Prompter
}

func (r recorder) Record(ctx context.Context, m core.Message, role Role) error {
	command, known := r.commands[role]
	if !known {
		return fmt.Errorf("no command records the %q role", role)
	}
	text := core.TextOf(m.Content)
	if text == "" {
		return errors.New("a message with no text cannot be recorded")
	}
	// A slash command and its argument are one line, so a message's own line
	// breaks would end the command halfway through.
	prompt := core.Text(command + " " + strings.ReplaceAll(text, "\n", " "))
	return r.prompter.Prompt(ctx, m.Conversation, prompt, r.discard(m.Conversation))
}

// discard drops what the agent answers a record with: the record is not a turn,
// and its reply belongs to no contact.
//
// ponytail: ACP carries a slash command as a normal prompt, so momo cannot tell a
// supported command from an unknown-command reply. This debug line is the only
// diagnosis this path has.
func (r recorder) discard(conversation string) core.Emit {
	return func(content []core.ContentBlock) error {
		r.log.Debug("record output discarded", "conversation", conversation, "text", core.TextOf(content))
		return nil
	}
}

// Qualifying prefixes a recorded conversation with the channel's configured name,
// the way a received or sent message is qualified, so one conversation is one
// session whichever direction reaches the agent. A disabled extension stays nil,
// because a wrapper holding nothing still looks like a recorder to the channel
// that checks.
func Qualifying(name string, r Recorder) Recorder {
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
