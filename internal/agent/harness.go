package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

type settings struct {
	Command []string `yaml:"command"`
	DataDir string   `yaml:"data_dir"`
	// A pointer tells an absent setting, which takes the default, from a value the
	// operator set, which is refused when it cannot bound a turn.
	TurnTimeout *time.Duration `yaml:"turn_timeout"`
	// The record commands are optional: an agent used only by a channel that never
	// records, such as ACP, needs neither.
	RecordUserMessageCommand      string `yaml:"record_user_message_command"`
	RecordAssistantMessageCommand string `yaml:"record_assistant_message_command"`
}

// Harness runs a turn on an agent subprocess that speaks ACP on its stdin and
// stdout.
type Harness struct {
	log     *slog.Logger
	command []string
	dataDir string
	timeout time.Duration
	// recordCommands holds the command that records a message of each role, as the
	// operator configured it. A role with no command records nothing.
	recordCommands map[core.Role]string

	mu    sync.Mutex
	turns map[string]chan struct{}
}

// New reads the agent block and refuses a configuration momo cannot serve with,
// before the process serves anything.
func New(log *slog.Logger, decode func(any) error) (*Harness, error) {
	var s settings
	if err := decode(&s); err != nil {
		return nil, err
	}
	if len(s.Command) == 0 {
		return nil, errors.New("command is required")
	}
	if s.DataDir == "" {
		return nil, errors.New("data_dir is required")
	}
	timeout := 30 * time.Minute
	if s.TurnTimeout != nil {
		if *s.TurnTimeout <= 0 {
			return nil, errors.New("turn_timeout must be positive")
		}
		timeout = *s.TurnTimeout
	}
	// ACP v1 requires an absolute cwd in session/new, and the conversation
	// directory is that cwd.
	dataDir, err := filepath.Abs(s.DataDir)
	if err != nil {
		return nil, fmt.Errorf("data_dir %q: %w", s.DataDir, err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("data_dir %q: %w", dataDir, err)
	}
	return &Harness{
		log:     log,
		command: s.Command,
		dataDir: dataDir,
		timeout: timeout,
		recordCommands: map[core.Role]string{
			core.RoleUser:      s.RecordUserMessageCommand,
			core.RoleAssistant: s.RecordAssistantMessageCommand,
		},
		turns: map[string]chan struct{}{},
	}, nil
}

// Turn runs one turn of one conversation and delivers its reply.
func (h *Harness) Turn(ctx context.Context, m core.Message, emit core.Emit) error {
	return h.exchange(ctx, m.Conversation, m.Content, emit)
}

// Record puts a message momo did not produce into the conversation's session, with
// the command the operator configured for the role. It runs as a turn does, so a
// record and a turn of one conversation never run at the same time.
//
// ponytail: an agent answers an unknown command as it answers a known one, so momo
// cannot tell a stored message from a command the agent does not serve. What the
// agent streamed is in the debug log, which is where a configured but unsupported
// command is diagnosed.
func (h *Harness) Record(ctx context.Context, m core.Message, role core.Role) error {
	command := h.recordCommands[role]
	if command == "" {
		return fmt.Errorf("no command is configured to record a %s message", role)
	}
	text := core.TextOf(m.Content)
	if text == "" {
		return errors.New("a record needs text, and the message carries none")
	}
	// The command and its argument stay on one line, so a newline in the message
	// becomes a space.
	prompt := core.Text(command + " " + strings.ReplaceAll(text, "\n", " "))
	discard := func(content []core.ContentBlock) error {
		h.log.Debug("record output", "conversation", m.Conversation, "text", core.TextOf(content))
		return nil
	}
	return h.exchange(ctx, m.Conversation, prompt, discard)
}

// exchange sends one prompt to the conversation's session. The timeout bounds
// everything it does, the wait for the conversation included, so a message behind a
// stopped turn is released as well.
func (h *Harness) exchange(ctx context.Context, conversation string, prompt []core.ContentBlock, emit core.Emit) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	release, err := h.acquire(ctx, conversation)
	if err != nil {
		return fmt.Errorf("waiting for the conversation: %w", err)
	}
	defer release()
	dir := filepath.Join(h.dataDir, dirName(conversation))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return h.run(ctx, dir, prompt, emit)
}

// acquire holds the conversation for one turn or one record. Each conversation has
// a channel of capacity one, so exchanges of different conversations never wait for
// each other.
//
// ponytail: the map keeps one entry for each conversation it has ever seen, so it
// grows with the contact base. PR 5's actor for each contact, with a FIFO inbox,
// replaces this lock and its map.
func (h *Harness) acquire(ctx context.Context, conversation string) (func(), error) {
	h.mu.Lock()
	turn, known := h.turns[conversation]
	if !known {
		turn = make(chan struct{}, 1)
		h.turns[conversation] = turn
	}
	h.mu.Unlock()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
