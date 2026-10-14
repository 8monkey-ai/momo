package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

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
		turns:   map[string]chan struct{}{},
	}, nil
}

// Turn runs one turn of one conversation. The timeout bounds everything the turn
// does, the wait for the conversation included, so a message behind a stopped
// turn is released as well.
func (h *Harness) Turn(ctx context.Context, m core.Message, emit core.Emit) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	release, err := h.acquire(ctx, m.Conversation)
	if err != nil {
		return fmt.Errorf("waiting for the conversation: %w", err)
	}
	defer release()
	dir, err := h.conversationDir(m.Conversation)
	if err != nil {
		return err
	}
	return h.run(ctx, dir, m.Content, emit)
}

// Save stores a stream where the conversation's agent process can read it.
func (h *Harness) Save(ctx context.Context, conversation, name string, r io.Reader) (string, string, error) {
	root, dir, err := h.openConversationDir(conversation)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = root.Close() }()

	temp, tempName, err := createTemp(root)
	if err != nil {
		return "", "", err
	}
	defer func() {
		_ = temp.Close()
		_ = root.Remove(tempName)
	}()
	if _, err := io.Copy(temp, contextReader{ctx: ctx, r: r}); err != nil {
		return "", "", fmt.Errorf("save %q: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if err := temp.Close(); err != nil {
		return "", "", fmt.Errorf("save %q: %w", name, err)
	}

	safeName, err := place(root, tempName, safeFileName(name))
	if err != nil {
		return "", "", fmt.Errorf("save %q: %w", name, err)
	}
	absolute := filepath.Join(dir, safeName)
	uriPath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" {
		uriPath = "/" + uriPath
	}
	return safeName, (&url.URL{Scheme: "file", Path: uriPath}).String(), nil
}

func (h *Harness) conversationDir(conversation string) (string, error) {
	root, dir, err := h.openConversationDir(conversation)
	if root != nil {
		_ = root.Close()
	}
	return dir, err
}

func (h *Harness) openConversationDir(conversation string) (*os.Root, string, error) {
	data, err := os.OpenRoot(h.dataDir)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = data.Close() }()
	name := dirName(conversation)
	if err := data.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, "", err
	}
	root, err := data.OpenRoot(name)
	if err != nil {
		return nil, "", err
	}
	return root, filepath.Join(h.dataDir, name), nil
}

func createTemp(root *os.Root) (*os.File, string, error) {
	for {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := ".momo-" + hex.EncodeToString(random[:]) + ".tmp"
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return file, name, err
	}
}

func safeFileName(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "attachment"
	}
	return name
}

func place(root *os.Root, tempName, name string) (string, error) {
	for copy := 1; ; copy++ {
		candidate := numberedName(name, copy)
		err := root.Link(tempName, candidate)
		if err == nil {
			return candidate, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
}

func numberedName(name string, copy int) string {
	if copy == 1 {
		return name
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem, ext = name, ""
	}
	return fmt.Sprintf("%s-%d%s", stem, copy, ext)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// acquire holds the conversation for one turn. Each conversation has a channel of
// capacity one, so turns of different conversations never wait for each other.
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
