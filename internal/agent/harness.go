package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

type settings struct {
	Command []string `yaml:"command"`
	DataDir string   `yaml:"data_dir"`
	// A pointer tells an absent setting, which takes the default, from a value the
	// operator set, which is refused when it cannot bound a turn.
	TurnTimeout *time.Duration `yaml:"turn_timeout"`
}

// Harness runs a turn on an agent subprocess that speaks ACP on its stdin and
// stdout.
type Harness struct {
	log     *slog.Logger
	command []string
	dataDir string
	timeout time.Duration
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
	}, nil
}

// Turn runs one turn of one conversation. The timeout bounds everything the turn
// does.
func (h *Harness) Turn(ctx context.Context, m core.Message, emit core.Emit) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	dir := filepath.Join(h.dataDir, dirName(m.Conversation))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return h.run(ctx, dir, m.Content, emit)
}
