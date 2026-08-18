package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/core"
)

// stub is the path of the built stub agent, shared by every test in the package.
var stub string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "stubagent")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stub = filepath.Join(dir, "stubagent")
	if out, err := exec.Command("go", "build", "-o", stub, "./testdata/stubagent").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the stub agent: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func decoder(body string) func(any) error {
	return func(v any) error {
		dec := yaml.NewDecoder(strings.NewReader(body))
		dec.KnownFields(true)
		if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
}

func harness(t *testing.T) (*Harness, string) {
	t.Helper()
	root := t.TempDir()
	h, err := New(discard(), decoder(fmt.Sprintf("command: [%q]\ndata_dir: %q\n", stub, root)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, root
}

func TestTurnAnswersWithTheContentTheAgentStreamed(t *testing.T) {
	h, _ := harness(t)
	content, err := h.Turn(context.Background(), core.Message{Conversation: "respondio:1", Content: core.Text("hi")})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	want := []core.ContentBlock{
		{Type: "text", Text: "hello from"},
		{Type: "text", Text: "the stub agent"},
	}
	if len(content) != len(want) {
		t.Fatalf("content = %+v, want %+v", content, want)
	}
	for i, block := range content {
		if block != want[i] {
			t.Fatalf("content[%d] = %+v, want %+v", i, block, want[i])
		}
	}
}

func TestTheSubprocessIsGoneAfterTheTurn(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("STUBAGENT_PID_FILE", pidFile)
	h, _ := harness(t)
	if _, err := h.Turn(context.Background(), core.Message{Conversation: "respondio:1", Content: core.Text("hi")}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the stub agent reported no pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid file holds %q: %v", raw, err)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := p.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("process %d is still running after the turn", pid)
	}
}

func TestTheConversationDirectoryExistsAndIsEmpty(t *testing.T) {
	h, root := harness(t)
	if _, err := h.Turn(context.Background(), core.Message{Conversation: "respondio:1", Content: core.Text("hi")}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, dirName("respondio:1")))
	if err != nil {
		t.Fatalf("the conversation directory is missing: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the conversation directory holds %d entries, want 0", len(entries))
	}
}

// TestTurnsOfDifferentConversationsRunAtTheSameTime releases no prompt before
// both prompts have arrived, so an implementation that serialises the two turns
// fails by deadlock and not by a measurement of time.
func TestTurnsOfDifferentConversationsRunAtTheSameTime(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	t.Setenv("STUBAGENT_SYNC_ADDR", l.Addr().String())
	h, _ := harness(t)

	failed := make(chan error, 2)
	for _, conversation := range []string{"respondio:1", "respondio:2"} {
		go func() {
			_, err := h.Turn(context.Background(), core.Message{Conversation: conversation, Content: core.Text("hi")})
			failed <- err
		}()
	}

	held := make([]net.Conn, 0, 2)
	for range 2 {
		conn, err := l.Accept()
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
			t.Fatalf("reading the prompt's mark: %v", err)
		}
		held = append(held, conn)
	}
	for _, conn := range held {
		if _, err := conn.Write([]byte{'.'}); err != nil {
			t.Fatalf("releasing a prompt: %v", err)
		}
		_ = conn.Close()
	}
	for range 2 {
		if err := <-failed; err != nil {
			t.Fatalf("Turn: %v", err)
		}
	}
}

// TestTheSessionWorksInAnAbsoluteDirectory pins what ACP v1 requires of cwd: an
// operator who configures a relative data_dir still gets an absolute path on the
// wire.
func TestTheSessionWorksInAnAbsoluteDirectory(t *testing.T) {
	cwdFile := filepath.Join(t.TempDir(), "cwd")
	t.Setenv("STUBAGENT_CWD_FILE", cwdFile)
	t.Chdir(t.TempDir())
	h, err := New(discard(), decoder(fmt.Sprintf("command: [%q]\ndata_dir: conversations\n", stub)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := h.Turn(context.Background(), core.Message{Conversation: "respondio:1", Content: core.Text("hi")}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	cwd, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatalf("the stub agent reported no cwd: %v", err)
	}
	if !filepath.IsAbs(string(cwd)) {
		t.Fatalf("session/new cwd = %q, want an absolute path", cwd)
	}
}

func TestNewRefusesAnUnusableConfiguration(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"no agent block": "",
		"no command":     fmt.Sprintf("data_dir: %q\n", t.TempDir()),
		"no data_dir":    fmt.Sprintf("command: [%q]\n", stub),
		"unusable data_dir": fmt.Sprintf("command: [%q]\ndata_dir: %q\n", stub,
			filepath.Join(file, "under-a-file")),
		"turn_timeout of zero": fmt.Sprintf("command: [%q]\ndata_dir: %q\nturn_timeout: 0s\n", stub, t.TempDir()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(discard(), decoder(body)); err == nil {
				t.Fatal("New succeeded, want an error")
			}
		})
	}
}
