// Package agent runs one turn of a conversation with an agent harness: momo
// starts the harness as a subprocess and speaks ACP v1 to it as the client, over
// the subprocess's standard input and output.
//
// This is the only way momo reaches a harness, so there is one implementation.
// Every difference between harnesses is a capability the harness advertises, and
// never its name or its version.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

var _ core.Agent = (*Agent)(nil)

type settings struct {
	Command     []string       `yaml:"command"`
	DataDir     string         `yaml:"data_dir"`
	TurnTimeout *time.Duration `yaml:"turn_timeout"`
}

// Agent starts one harness subprocess per turn and holds the settings every turn
// runs with.
type Agent struct {
	command []string
	dataDir string
	timeout time.Duration
	log     *slog.Logger

	mu sync.Mutex
	// ponytail: one lock per conversation, kept for the lifetime of the process;
	// PR 5 replaces it with an actor per contact.
	locks map[string]chan struct{}
}

// New decodes the agent's own settings block and refuses to build an agent that
// cannot run a turn. decode is an unnamed func type so that this package depends
// on neither the configuration nor the channels.
func New(decode func(any) error, log *slog.Logger) (*Agent, error) {
	var s settings
	if err := decode(&s); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	if len(s.Command) == 0 {
		return nil, errors.New("agent: command is required")
	}
	if s.DataDir == "" {
		return nil, errors.New("agent: data_dir is required")
	}
	// ACP v1 requires an absolute cwd in every session request.
	dataDir, err := filepath.Abs(s.DataDir)
	if err != nil {
		return nil, fmt.Errorf("agent: data_dir: %w", err)
	}
	timeout := 30 * time.Minute
	if s.TurnTimeout != nil {
		timeout = *s.TurnTimeout
	}
	if timeout <= 0 {
		return nil, errors.New("agent: turn_timeout must be positive")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: data_dir: %w", err)
	}
	return &Agent{
		command: s.Command,
		dataDir: dataDir,
		timeout: timeout,
		log:     log,
		locks:   map[string]chan struct{}{},
	}, nil
}

// Turn runs one turn of one conversation: it starts a harness, resumes the
// session of the conversation or creates one, prompts it, and returns the reply
// the harness streamed. The subprocess is gone when Turn returns, on every path.
func (a *Agent) Turn(ctx context.Context, conversation string, prompt []core.ContentBlock) ([]core.ContentBlock, error) {
	// The limit covers the whole turn, the wait for the conversation included, so
	// a message behind a stopped turn is released by the same limit.
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	release, err := a.lock(ctx, conversation)
	if err != nil {
		return nil, fmt.Errorf("conversation %s: %w", conversation, err)
	}
	defer release()

	dir := filepath.Join(a.dataDir, directoryName(conversation))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	h, err := a.start(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer h.stop()

	capabilities, err := h.initialize(ctx)
	if err != nil {
		return nil, err
	}
	session, err := h.session(ctx, capabilities, dir)
	if err != nil {
		return nil, err
	}
	return h.prompt(ctx, session, prompt)
}

// lock serialises the turns of one conversation. Turns of different
// conversations never wait for each other.
func (a *Agent) lock(ctx context.Context, conversation string) (func(), error) {
	a.mu.Lock()
	held, known := a.locks[conversation]
	if !known {
		held = make(chan struct{}, 1)
		a.locks[conversation] = held
	}
	a.mu.Unlock()
	select {
	case held <- struct{}{}:
		return func() { <-held }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
