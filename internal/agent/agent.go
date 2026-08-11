// Package agent runs a conversation's turns on an ACP agent harness: momo
// spawns the harness as a subprocess, drives it over its stdin and stdout, and
// terminates it when the turn ends. The harness works in a directory of its own
// per conversation and persists its sessions there, which is how momo finds the
// conversation's session again after the subprocess is gone.
//
// Hand-written against Agent Client Protocol v1, the version the inbound ACP
// channel serves. There is one way to reach a harness and therefore one
// implementation: agents differ only in the capabilities they advertise.
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
	Command   []string      `yaml:"command"`
	DataDir   string        `yaml:"data_dir"`
	Template  string        `yaml:"template"`
	StopGrace time.Duration `yaml:"stop_grace"`
}

// Harness is the agent momo runs turns on, reached by spawning it.
type Harness struct {
	command   []string
	dataDir   string
	template  string
	stopGrace time.Duration
	log       *slog.Logger
}

// New decodes the agent's settings block and refuses anything that could not
// serve a turn, before momo serves at all.
func New(decode func(v any) error, log *slog.Logger) (*Harness, error) {
	s := settings{StopGrace: 10 * time.Second}
	if err := decode(&s); err != nil {
		return nil, err
	}
	if len(s.Command) == 0 {
		return nil, errors.New("command is required")
	}
	if s.DataDir == "" {
		return nil, errors.New("data_dir is required")
	}
	// A non-positive grace would terminate a harness before it can persist.
	if s.StopGrace <= 0 {
		return nil, errors.New("stop_grace must be positive")
	}
	// ACP requires an absolute session working directory.
	dataDir, err := filepath.Abs(s.DataDir)
	if err != nil {
		return nil, err
	}
	template := s.Template
	if template != "" {
		if template, err = filepath.Abs(template); err != nil {
			return nil, err
		}
	}
	return &Harness{command: s.Command, dataDir: dataDir, template: template, stopGrace: s.StopGrace, log: log}, nil
}

// Turn spawns the harness in the conversation's directory, prompts the session
// belonging to that conversation, and returns everything the harness streamed.
func (h *Harness) Turn(ctx context.Context, conversation string, prompt []core.ContentBlock) ([]core.ContentBlock, error) {
	cwd, err := h.workspace(conversation)
	if err != nil {
		return nil, err
	}
	s, err := h.spawn(ctx, conversation, cwd)
	if err != nil {
		return nil, err
	}
	defer s.stop()
	return s.prompt(ctx, prompt)
}

// workspace is the conversation's own directory, created on demand and seeded
// from the template so the harness finds its project configuration there.
//
// The qualified conversation identity is the directory name: both channels emit
// contact ids that are safe path segments, which this trusts.
func (h *Harness) workspace(conversation string) (string, error) {
	cwd := filepath.Join(h.dataDir, conversation)
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return "", err
	}
	return cwd, h.seed(cwd)
}

// seed links the template's entries into cwd, keeping whatever is already there
// and skipping the repository the template may live in.
func (h *Harness) seed(cwd string) error {
	if h.template == "" {
		return nil
	}
	entries, err := os.ReadDir(h.template)
	if err != nil {
		return fmt.Errorf("agent template: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		link := filepath.Join(cwd, entry.Name())
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join(h.template, entry.Name()), link); err != nil {
			return err
		}
	}
	return nil
}
