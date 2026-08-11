package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/8monkey-ai/momo/internal/core"
)

// stubAgent is the ACP agent every test spawns, built once for the package.
var stubAgent string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "stubagent")
	if err != nil {
		panic(err)
	}
	stubAgent = filepath.Join(dir, "stubagent")
	out, err := exec.Command("go", "build", "-o", stubAgent, "github.com/8monkey-ai/momo/internal/agent/stubagent").CombinedOutput()
	if err != nil {
		panic("building the stub agent: " + err.Error() + ": " + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func configure(command []string, dataDir, template string) func(any) error {
	return func(v any) error {
		s, ok := v.(*settings)
		if !ok {
			return nil
		}
		s.Command = command
		s.DataDir = dataDir
		s.Template = template
		return nil
	}
}

func newHarness(t *testing.T, args ...string) *Harness {
	t.Helper()
	h, err := New(configure(append([]string{stubAgent}, args...), t.TempDir(), ""), discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func turn(t *testing.T, h *Harness, conversation, text string) string {
	t.Helper()
	content, err := h.Turn(context.Background(), conversation, core.Text(text))
	if err != nil {
		t.Fatalf("Turn(%q): %v", conversation, err)
	}
	return core.TextOf(content)
}

var sessionID = regexp.MustCompile(`session=(\S+)`)

func sessionOf(t *testing.T, reply string) string {
	t.Helper()
	m := sessionID.FindStringSubmatch(reply)
	if m == nil {
		t.Fatalf("reply %q does not name the session the agent ran", reply)
	}
	return m[1]
}

func TestTurnDeliversWhatTheAgentStreamed(t *testing.T) {
	got := turn(t, newHarness(t), "respondio:7", "hello")

	if !strings.HasSuffix(got, "echo:hello") {
		t.Fatalf("reply = %q, want every streamed chunk, ending in the agent's echo", got)
	}
}

func TestASecondTurnResumesTheSessionTheFirstCreated(t *testing.T) {
	h := newHarness(t)

	first := sessionOf(t, turn(t, h, "respondio:7", "one"))
	second := sessionOf(t, turn(t, h, "respondio:7", "two"))
	other := sessionOf(t, turn(t, h, "respondio:8", "one"))

	if first != second {
		t.Errorf("sessions = %q then %q, want the conversation's second turn to resume the first", first, second)
	}
	if other == first {
		t.Errorf("both conversations ran session %q, want one session per conversation", other)
	}
}

func TestAnAgentThatListsAndLoadsNothingGetsANewSessionEveryTurn(t *testing.T) {
	h := newHarness(t, "-resume=false")

	first := sessionOf(t, turn(t, h, "respondio:7", "one"))
	second := sessionOf(t, turn(t, h, "respondio:7", "two"))

	if first == second {
		t.Fatalf("both turns ran session %q, want a new session when the agent advertises neither listing nor loading", first)
	}
}

func TestPermissionIsApprovedWithAnOptionTheAgentOffered(t *testing.T) {
	got := turn(t, newHarness(t), "respondio:7", "may I, permission")

	if !strings.Contains(got, "permission:yes") {
		t.Fatalf("reply = %q, want the agent's allowing option to have been selected", got)
	}
}

func TestTheSubprocessIsGoneWhenTheTurnEnds(t *testing.T) {
	h := newHarness(t)
	turn(t, h, "respondio:7", "hello")

	raw, err := os.ReadFile(filepath.Join(h.dataDir, "respondio:7", "stubagent.pid"))
	if err != nil {
		t.Fatalf("the agent left no pid behind: %v", err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("signalling pid %d after the turn = %v, want the process reaped", pid, err)
	}
}

func TestTheConversationDirectoryIsSeededWithoutClobbering(t *testing.T) {
	template := t.TempDir()
	if err := os.WriteFile(filepath.Join(template, "AGENTS.md"), []byte("from the template"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(template, "kept.md"), []byte("from the template"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "respondio:7"), 0o700); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(dataDir, "respondio:7", "kept.md")
	if err := os.WriteFile(kept, []byte("already here"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := New(configure([]string{stubAgent}, dataDir, template), discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	turn(t, h, "respondio:7", "hello")

	seeded, err := os.ReadFile(filepath.Join(dataDir, "respondio:7", "AGENTS.md"))
	if err != nil || string(seeded) != "from the template" {
		t.Errorf("AGENTS.md = %q, %v, want the template's copy in the session directory", seeded, err)
	}
	survived, err := os.ReadFile(kept)
	if err != nil || string(survived) != "already here" {
		t.Errorf("kept.md = %q, %v, want the file that was already there", survived, err)
	}
}

func TestNewRefusesSettingsThatCannotServeATurn(t *testing.T) {
	for name, decode := range map[string]func(any) error{
		"no command":  configure(nil, "/tmp/momo", ""),
		"no data dir": configure([]string{"pi"}, "", ""),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(decode, discard()); err == nil {
				t.Fatal("New succeeded, want it to refuse before serving")
			}
		})
	}
}
