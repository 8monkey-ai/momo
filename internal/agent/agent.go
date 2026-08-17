// Package agent runs one turn of a conversation on an ACP agent harness. momo
// starts the harness as a subprocess, speaks ACP v1 over its stdin and stdout,
// and stops it when the turn is over. This is momo's one way to reach a harness.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	Command []string      `yaml:"command"`
	DataDir string        `yaml:"data_dir"`
	Timeout time.Duration `yaml:"turn_timeout"`
}

// Agent answers a message with one turn of an agent harness.
type Agent struct {
	command []string
	dataDir string
	timeout time.Duration
	log     *slog.Logger

	mu    sync.Mutex
	turns map[string]chan struct{}
}

// New decodes the agent block and reports a missing or unusable setting, so momo
// stops before it serves.
func New(decode func(v any) error, log *slog.Logger) (*Agent, error) {
	s := settings{Timeout: 30 * time.Minute}
	if err := decode(&s); err != nil {
		return nil, err
	}
	if len(s.Command) == 0 {
		return nil, errors.New("command is required")
	}
	if s.DataDir == "" {
		return nil, errors.New("data_dir is required")
	}
	if s.Timeout <= 0 {
		return nil, errors.New("turn_timeout must be positive")
	}
	// ACP v1 requires an absolute cwd, and the directory of a conversation is what
	// makes its session findable, so the root cannot depend on where momo was
	// started.
	dataDir, err := filepath.Abs(s.DataDir)
	if err != nil {
		return nil, err
	}
	return &Agent{
		command: s.Command,
		dataDir: dataDir,
		timeout: s.Timeout,
		log:     log,
		turns:   map[string]chan struct{}{},
	}, nil
}

// Turn answers the prompt of one conversation. The timeout covers the whole
// turn, from the wait for the conversation to the answer of the harness, so a
// message waiting behind a turn that stopped answering is released as well.
func (a *Agent) Turn(ctx context.Context, conversation string, prompt []core.ContentBlock, reply core.Reply) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	release, err := a.hold(ctx, conversation)
	if err != nil {
		return fmt.Errorf("waiting for conversation %s: %w", conversation, err)
	}
	defer release()

	dir := a.directory(conversation)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return a.run(ctx, dir, prompt, reply)
}

func (a *Agent) run(ctx context.Context, dir string, prompt []core.ContentBlock, reply core.Reply) error {
	h, err := start(ctx, a.command, dir, a.log)
	if err != nil {
		return fmt.Errorf("starting the harness: %w", err)
	}
	defer h.stop()

	sessions, err := h.initialize(ctx)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	session, err := h.session(ctx, sessions, dir)
	if err != nil {
		return fmt.Errorf("choosing a session: %w", err)
	}
	// Content the harness streamed while momo prepared the session belongs to an
	// earlier turn.
	h.discard()
	if err := h.prompt(ctx, session, prompt); err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	return reply(ctx, h.collected())
}

// hold gives the conversation to one turn at a time. Conversations hold nothing
// in common, so a turn of another conversation never waits here.
//
// ponytail: the map keeps one channel per conversation momo has ever answered,
// so it grows for the life of the process, and messages of one conversation wait
// in no order. PR 5 replaces this with an actor per contact that holds a FIFO
// inbox.
func (a *Agent) hold(ctx context.Context, conversation string) (func(), error) {
	a.mu.Lock()
	held, known := a.turns[conversation]
	if !known {
		held = make(chan struct{}, 1)
		a.turns[conversation] = held
	}
	a.mu.Unlock()

	select {
	case held <- struct{}{}:
		return func() { <-held }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// directory is the one place a conversation identity becomes a path. Every
// character that is not a letter, a digit or a hyphen becomes a hyphen, so no
// channel has to be trusted to supply a safe name, and no channel added later
// has to be examined for it. The digest of the identity keeps two identities
// that give the same readable name, and two that differ in case only, in two
// directories.
func (a *Agent) directory(conversation string) string {
	sum := sha256.Sum256([]byte(conversation))
	return filepath.Join(a.dataDir, strings.Map(safe, conversation)+"-"+hex.EncodeToString(sum[:4]))
}

func safe(r rune) rune {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		return r
	default:
		return '-'
	}
}
