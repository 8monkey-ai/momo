// Package agent runs a conversation's turns on an ACP agent harness: one
// subprocess per turn, spawned in the conversation's own directory, prompted
// once, and terminated. It is the only way momo reaches an agent, so there is
// one implementation of it and no variant to select.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/8monkey-ai/momo/internal/core"
)

type settings struct {
	Command  []string `yaml:"command"`
	DataDir  string   `yaml:"data_dir"`
	Template string   `yaml:"template"`
}

// Agent spawns a harness per turn. It holds what every turn needs and nothing a
// turn produces.
type Agent struct {
	command  []string
	root     string
	template string
	log      *slog.Logger

	mu sync.Mutex
	// turns holds one lock per conversation.
	//
	// ponytail: the map grows with the conversations momo has ever served, and a
	// lock serializes turns without ordering them; PR 5 replaces both with a
	// per-contact actor and a FIFO inbox.
	turns map[string]*sync.Mutex
}

// New decodes the agent's configuration block and refuses anything it cannot run
// with: a harness momo cannot spawn is a misconfiguration, not a degraded mode.
func New(decode func(v any) error, log *slog.Logger) (*Agent, error) {
	var s settings
	if err := decode(&s); err != nil {
		return nil, err
	}
	if len(s.Command) == 0 {
		return nil, errors.New("command is required")
	}
	if _, err := exec.LookPath(s.Command[0]); err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	// ACP requires an absolute working directory, and every session directory is
	// built under this one.
	if !filepath.IsAbs(s.DataDir) {
		return nil, errors.New("data_dir is required and must be an absolute path")
	}
	if err := os.MkdirAll(s.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	if s.Template != "" {
		if _, err := os.Stat(s.Template); err != nil {
			return nil, fmt.Errorf("template: %w", err)
		}
	}
	return &Agent{
		command:  s.Command,
		root:     s.DataDir,
		template: s.Template,
		log:      log,
		turns:    map[string]*sync.Mutex{},
	}, nil
}

// Turn runs one turn on a harness of its own. Turns of one conversation run one
// at a time; turns of different conversations do not wait for each other.
func (a *Agent) Turn(ctx context.Context, conversation string, prompt []core.ContentBlock) ([]core.ContentBlock, error) {
	lock := a.lockFor(conversation)
	lock.Lock()
	defer lock.Unlock()

	dir, err := a.workspace(conversation)
	if err != nil {
		return nil, err
	}
	h, err := a.spawn(ctx, dir)
	if err != nil {
		return nil, err
	}
	// Termination is on every exit path, so no harness is left behind.
	defer h.stop()
	return h.turn(ctx, dir, prompt)
}

func (a *Agent) lockFor(conversation string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	lock, known := a.turns[conversation]
	if !known {
		lock = &sync.Mutex{}
		a.turns[conversation] = lock
	}
	return lock
}

// workspace is the conversation's own directory, created on first use and seeded
// from the template so the harness finds its project configuration where it
// works. Seeding happens when the directory appears, so nothing already there is
// ever replaced.
func (a *Agent) workspace(conversation string) (string, error) {
	dir := filepath.Join(a.root, directoryName(conversation))
	err := os.Mkdir(dir, 0o700)
	if errors.Is(err, fs.ErrExist) {
		return dir, nil
	}
	if err != nil {
		return "", err
	}
	if a.template == "" {
		return dir, nil
	}
	if err := os.CopyFS(dir, os.DirFS(a.template)); err != nil {
		return "", fmt.Errorf("seeding %s from %s: %w", dir, a.template, err)
	}
	return dir, nil
}

// directoryName turns a conversation identity into one directory name. The
// identity carries a contact id a channel received from the outside, so it is
// encoded rather than trusted: everything outside a conservative set becomes a
// percent escape, which keeps a separator or a "..", and any two different
// identities, apart. Encoding here is why no channel has to be reviewed for
// emitting path-safe ids.
func directoryName(conversation string) string {
	var name strings.Builder
	for _, b := range []byte(conversation) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_' {
			name.WriteByte(b)
			continue
		}
		fmt.Fprintf(&name, "%%%02X", b)
	}
	return name.String()
}
