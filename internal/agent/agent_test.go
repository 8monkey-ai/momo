package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// stubPath is the stub harness, built once for the whole test binary.
var stubPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "stubagent")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stubPath = filepath.Join(dir, "stubagent")
	build := exec.Command("go", "build", "-o", stubPath, "./testdata/stubagent")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the stub agent: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type harnessAgent struct {
	*Agent
	dataDir string
}

func newAgent(t *testing.T, turnTimeout string, stubArgs ...string) harnessAgent {
	t.Helper()
	dataDir := t.TempDir()
	block, err := yaml.Marshal(map[string]any{
		"command":      append([]string{stubPath}, stubArgs...),
		"data_dir":     dataDir,
		"turn_timeout": turnTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(func(v any) error { return yaml.Unmarshal(block, v) }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return harnessAgent{Agent: a, dataDir: dataDir}
}

// turn runs one turn and reports the session id the stub streamed first and the
// text of the rest of the reply.
func turn(t *testing.T, a harnessAgent, conversation, text string) (session, reply string) {
	t.Helper()
	content, err := a.Turn(context.Background(), conversation, core.Text(text))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("the turn returned no content")
	}
	return content[0].Text, core.TextOf(content[1:])
}

// harnessPID is the process id the stub wrote into the conversation directory.
func harnessPID(t *testing.T, a harnessAgent, conversation string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(a.dataDir, directoryName(conversation), "agent.pid"))
	if err != nil {
		t.Fatalf("reading the stub's pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestTurnPromptsTheHarnessAndReturnsTheStreamedReply(t *testing.T) {
	a := newAgent(t, "30s")
	session, reply := turn(t, a, "respondio:123", "hello harness")
	if session == "" {
		t.Error("the harness streamed no session id")
	}
	if reply != "hello harness" {
		t.Errorf("reply = %q, want %q", reply, "hello harness")
	}
}

func TestSecondTurnResumesTheSessionTheFirstTurnCreated(t *testing.T) {
	a := newAgent(t, "30s")
	first, _ := turn(t, a, "respondio:123", "one")
	second, _ := turn(t, a, "respondio:123", "two")
	if first != second {
		t.Errorf("session ids = %q and %q, want the second turn to resume the first session", first, second)
	}
	other, _ := turn(t, a, "respondio:456", "three")
	if other == first {
		t.Errorf("session id = %q for both conversations, want a session of its own", other)
	}
}

func TestResumedTurnReturnsThisTurnsContentOnly(t *testing.T) {
	a := newAgent(t, "30s", "-stream-on-resume")
	turn(t, a, "respondio:123", "the earlier turn")
	content, err := a.Turn(context.Background(), "respondio:123", core.Text("this turn"))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if strings.Contains(core.TextOf(content), "the earlier turn") {
		t.Errorf("reply = %q, want the content of this turn only", core.TextOf(content))
	}
	if len(content) != 2 || content[1].Text != "this turn" {
		t.Errorf("reply = %v, want the session id and %q", content, "this turn")
	}
}

func TestHarnessWithoutListingOrResumptionGetsANewSessionPerTurn(t *testing.T) {
	a := newAgent(t, "30s", "-no-session-capabilities")
	first, reply := turn(t, a, "respondio:123", "one")
	second, _ := turn(t, a, "respondio:123", "two")
	if reply != "one" {
		t.Errorf("reply = %q, want %q", reply, "one")
	}
	if first == second {
		t.Errorf("session id = %q for both turns, want a new session per turn", first)
	}
}

func TestPermissionRequestIsApprovedWithTheFirstAllowingOption(t *testing.T) {
	a := newAgent(t, "30s", "-ask-permission")
	_, reply := turn(t, a, "respondio:123", "run it")
	if !strings.Contains(reply, "selected=allow-once") {
		t.Errorf("reply = %q, want the option of kind allow_once to be the selected one", reply)
	}
}

func TestSubprocessIsGoneAfterTheTurn(t *testing.T) {
	a := newAgent(t, "30s")
	turn(t, a, "respondio:123", "hello")
	assertGone(t, harnessPID(t, a, "respondio:123"))
}

func TestTurnThatReachesTheTimeoutFailsAndFreesTheConversation(t *testing.T) {
	a := newAgent(t, "300ms", "-never-answer-prompt")
	if _, err := a.Turn(context.Background(), "respondio:123", core.Text("hello")); err == nil {
		t.Fatal("Turn succeeded, want a failure at the turn timeout")
	}
	first := harnessPID(t, a, "respondio:123")
	assertGone(t, first)
	// A second turn of the same conversation reaches its own subprocess, which is
	// only possible if the failed turn released the conversation.
	if _, err := a.Turn(context.Background(), "respondio:123", core.Text("hello again")); err == nil {
		t.Fatal("the second turn succeeded, want a failure at the turn timeout")
	}
	second := harnessPID(t, a, "respondio:123")
	if second == first {
		t.Errorf("pid = %d for both turns, want the second turn to have started a subprocess", second)
	}
	assertGone(t, second)
}

func TestPromptAnsweredWithAnErrorFailsTheTurn(t *testing.T) {
	a := newAgent(t, "30s", "-fail-prompt")
	content, err := a.Turn(context.Background(), "respondio:123", core.Text("hello"))
	if err == nil {
		t.Fatalf("Turn returned %v, want the harness's error", content)
	}
}

func assertGone(t *testing.T, pid int) {
	t.Helper()
	// Signal 0 checks for the process without disturbing it.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("signalling pid %d returned %v, want the process to be gone", pid, err)
	}
}
