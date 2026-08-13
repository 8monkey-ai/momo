package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/core"
)

var (
	buildOnce sync.Once
	stubPath  string
	buildErr  error
)

// stub builds the test-only ACP agent once for the package and reports the path
// of the binary. Nothing in the tests depends on an installed agent.
func stub(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		stubPath = filepath.Join(os.TempDir(), "momo-stubagent")
		out, err := exec.Command("go", "build", "-o", stubPath, "./testdata/stubagent").CombinedOutput()
		if err != nil {
			buildErr = errors.New(string(out))
		}
	})
	if buildErr != nil {
		t.Fatalf("building the stub agent: %v", buildErr)
	}
	return stubPath
}

// newAgent configures an agent that reaches the stub, with the behaviour the stub
// is told to show.
func newAgent(t *testing.T, behaviour string, timeout time.Duration) *Agent {
	t.Helper()
	command := []string{stub(t)}
	if behaviour != "" {
		command = []string{"/usr/bin/env", "STUB_BEHAVIOUR=" + behaviour, stub(t)}
	}
	dataDir := t.TempDir()
	decode := func(v any) error {
		s := v.(*settings)
		s.Command = command
		s.DataDir = dataDir
		s.Timeout = timeout
		return nil
	}
	a, err := New(decode, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// turn runs one turn and reports the text of the reply.
func turn(t *testing.T, a *Agent, conversation, text string) string {
	t.Helper()
	reply, err := runTurn(a, conversation, text)
	if err != nil {
		t.Fatalf("turn of %s: %v", conversation, err)
	}
	return reply
}

func runTurn(a *Agent, conversation, text string) (string, error) {
	var reply string
	err := a.Turn(context.Background(), conversation, core.Text(text), func(_ context.Context, content []core.ContentBlock) error {
		reply = core.TextOf(content)
		return nil
	})
	return reply, err
}

func TestATurnAnswersWithWhatTheHarnessStreamed(t *testing.T) {
	a := newAgent(t, "", time.Minute)

	if got := turn(t, a, "respondio:12345", "hello"); got != "turn 1: hello" {
		t.Fatalf("reply = %q, want the reply of the first turn", got)
	}
}

func TestASecondTurnResumesTheSessionOfTheConversation(t *testing.T) {
	a := newAgent(t, "", time.Minute)

	turn(t, a, "respondio:12345", "one")
	// The turn count lives in the session of the harness, so a second turn that
	// counts two resumed the session the first turn created.
	second := turn(t, a, "respondio:12345", "two")
	if second != "turn 2: two" {
		t.Fatalf("second reply = %q, want the second turn of the same session", second)
	}
	if strings.Contains(second, "one") {
		t.Fatalf("second reply = %q, want the content of this turn only", second)
	}
	// A different conversation is a different directory, so it is a different
	// session.
	if other := turn(t, a, "respondio:999", "hello"); other != "turn 1: hello" {
		t.Fatalf("reply of the other conversation = %q, want its own first turn", other)
	}
}

func TestAHarnessWithoutListingGetsANewSessionEveryTurn(t *testing.T) {
	a := newAgent(t, "no-sessions", time.Minute)

	turn(t, a, "respondio:12345", "one")
	if second := turn(t, a, "respondio:12345", "two"); second != "turn 1: two" {
		t.Fatalf("second reply = %q, want a first turn in a new session", second)
	}
}

func TestAPermissionRequestIsApprovedFromTheOptionsTheHarnessSupplied(t *testing.T) {
	a := newAgent(t, "permission", time.Minute)

	got := turn(t, a, "respondio:12345", "hello")
	if !strings.Contains(got, "permission allow") {
		t.Fatalf("reply = %q, want the option momo selected", got)
	}
}

// TestAFilesystemRequestIsRefused pins that momo advertises no filesystem and
// answers a request for one with method not found, so the harness works in its
// own directory with its own access.
func TestAFilesystemRequestIsRefused(t *testing.T) {
	a := newAgent(t, "filesystem", time.Minute)

	if got := turn(t, a, "respondio:12345", "hello"); !strings.Contains(got, "filesystem -32601") {
		t.Fatalf("reply = %q, want the method-not-found code momo answered with", got)
	}
}

func TestTheSubprocessIsGoneAfterTheTurn(t *testing.T) {
	a := newAgent(t, "", time.Minute)

	turn(t, a, "respondio:12345", "hello")

	if !gone(t, a, "respondio:12345") {
		t.Fatal("the harness is still running after the turn")
	}
}

func TestATurnThatReachesTheTimeoutFailsAndReleasesTheConversation(t *testing.T) {
	a := newAgent(t, "hang", 200*time.Millisecond)

	if _, err := runTurn(a, "respondio:12345", "hello"); err == nil {
		t.Fatal("the turn succeeded, want the timeout to fail it")
	}
	if !gone(t, a, "respondio:12345") {
		t.Fatal("the harness is still running after the failed turn")
	}
	// The conversation is free: a message behind the failed turn is not blocked.
	if _, err := runTurn(a, "respondio:12345", "again"); err == nil {
		t.Fatal("the second turn succeeded, want the same timeout to fail it")
	}
}

func TestATurnFailsWhenTheHarnessRefusesThePrompt(t *testing.T) {
	a := newAgent(t, "fail", time.Minute)

	if _, err := runTurn(a, "respondio:12345", "hello"); err == nil {
		t.Fatal("the turn succeeded, want the refusal of the harness to fail it")
	}
}

func TestATurnFailsWhenTheHarnessCannotStart(t *testing.T) {
	a := newAgent(t, "", time.Minute)
	a.command = []string{filepath.Join(t.TempDir(), "no-such-harness")}

	if _, err := runTurn(a, "respondio:12345", "hello"); err == nil {
		t.Fatal("the turn succeeded, want the failed start to fail it")
	}
}

func TestTurnsOfDifferentConversationsDoNotWaitForEachOther(t *testing.T) {
	a := newAgent(t, "", time.Minute)
	held, err := a.hold(context.Background(), "respondio:12345")
	if err != nil {
		t.Fatal(err)
	}
	defer held()

	done := make(chan error, 1)
	go func() {
		_, err := runTurn(a, "respondio:999", "hello")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("turn of the other conversation: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a turn of another conversation waited for the held conversation")
	}
}

func TestTheConversationDirectoryIsSafeAndEmpty(t *testing.T) {
	a := newAgent(t, "", time.Minute)

	for _, conversation := range []string{"respondio:../../escape", "acp:12345"} {
		dir := a.directory(conversation)
		if parent := filepath.Dir(dir); parent != a.dataDir {
			t.Fatalf("directory of %q is %q, want it under %q", conversation, dir, a.dataDir)
		}
		if strings.ContainsAny(filepath.Base(dir), `:./\`) {
			t.Fatalf("directory of %q is %q, want a plain name", conversation, dir)
		}
	}
	// momo creates the directory and puts nothing in it.
	dir := a.directory("acp:12345")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the conversation directory holds %d entries, want none", len(entries))
	}
}

func TestNewReportsAnUnusableConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block string
	}{
		{name: "no command", block: "data_dir: /tmp/momo\n"},
		{name: "no data_dir", block: "command: [\"agent\"]\n"},
		{name: "timeout of zero", block: "command: [\"agent\"]\ndata_dir: /tmp/momo\nturn_timeout: 0s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(yamlDecoder(tc.block), slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
				t.Fatal("New succeeded, want the missing setting reported")
			}
		})
	}
}

func TestNewDefaultsTheTurnTimeoutToThirtyMinutes(t *testing.T) {
	a, err := New(yamlDecoder("command: [\"agent\"]\ndata_dir: /tmp/momo\n"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.timeout != 30*time.Minute {
		t.Fatalf("turn_timeout = %v, want 30m", a.timeout)
	}
}

// gone reports whether the harness that ran for the conversation has exited. The
// stub records its own pid, so no external tool is needed.
func gone(t *testing.T, a *Agent, conversation string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(a.directory(conversation), "stub.pid"))
	if err != nil {
		t.Fatalf("the harness recorded no pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// Signal 0 reports whether the process exists; momo waited for it, so it is
	// reaped and no longer exists.
	return syscall.Kill(pid, 0) != nil
}

// yamlDecoder decodes an agent block the way the configuration file supplies it.
func yamlDecoder(block string) func(v any) error {
	return func(v any) error {
		dec := yaml.NewDecoder(strings.NewReader(block))
		dec.KnownFields(true)
		return dec.Decode(v)
	}
}
