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

// recordingHarness answers a harness configured with the record commands the test
// states, and nothing more.
func recordingHarness(t *testing.T, commands string) *Harness {
	t.Helper()
	body := fmt.Sprintf("command: [%q]\ndata_dir: %q\n", stub, t.TempDir()) + commands
	h, err := New(discard(), decoder(body))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// noSubprocess fails when the stub agent reported a pid, which it does as soon as
// it starts.
func noSubprocess(t *testing.T) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("STUBAGENT_PID_FILE", pidFile)
	t.Cleanup(func() {
		if _, err := os.Stat(pidFile); err == nil {
			t.Fatal("an agent subprocess was started, want none")
		}
	})
}

// TestRecordSendsTheConfiguredCommandOnOneLine pins the prompt a record produces:
// the command of the role, then the message text with every newline replaced, so
// the command keeps its argument.
func TestRecordSendsTheConfiguredCommandOnOneLine(t *testing.T) {
	for name, tc := range map[string]struct {
		commands string
		role     core.Role
		want     string
	}{
		"a user message": {
			commands: "record_user_message_command: /store-user\n",
			role:     core.RoleUser,
			want:     "/store-user hello there",
		},
		"an assistant message": {
			commands: "record_assistant_message_command: /store-assistant\n",
			role:     core.RoleAssistant,
			want:     "/store-assistant hello there",
		},
	} {
		t.Run(name, func(t *testing.T) {
			promptFile := filepath.Join(t.TempDir(), "prompt")
			t.Setenv("STUBAGENT_PROMPT_FILE", promptFile)
			h := recordingHarness(t, tc.commands)
			m := core.Message{Conversation: "respondio:1", Content: core.Text("hello\nthere")}

			if err := h.Record(context.Background(), m, tc.role); err != nil {
				t.Fatalf("Record: %v", err)
			}
			raw, err := os.ReadFile(promptFile)
			if err != nil {
				t.Fatalf("the stub agent got no prompt: %v", err)
			}
			if got := strings.TrimRight(string(raw), "\n"); got != tc.want {
				t.Fatalf("prompt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRecordWithoutItsCommandFails pins that a role momo has no command for is
// refused before an agent runs, so an operator who configures one command does not
// get an agent started for the other role.
func TestRecordWithoutItsCommandFails(t *testing.T) {
	for name, tc := range map[string]struct {
		commands string
		role     core.Role
	}{
		"no command at all":             {commands: "", role: core.RoleUser},
		"only the assistant command":    {commands: "record_assistant_message_command: /store-assistant\n", role: core.RoleUser},
		"only the user command":         {commands: "record_user_message_command: /store-user\n", role: core.RoleAssistant},
		"an empty command for the role": {commands: "record_user_message_command: \"\"\n", role: core.RoleUser},
	} {
		t.Run(name, func(t *testing.T) {
			noSubprocess(t)
			h := recordingHarness(t, tc.commands)
			m := core.Message{Conversation: "respondio:1", Content: core.Text("hello")}

			if err := h.Record(context.Background(), m, tc.role); err == nil {
				t.Fatal("Record succeeded, want an error naming the missing command")
			}
		})
	}
}

// TestRecordOfAMessageWithoutTextFails pins what this version records: text only,
// and an image carries none.
func TestRecordOfAMessageWithoutTextFails(t *testing.T) {
	noSubprocess(t)
	h := recordingHarness(t, "record_user_message_command: /store-user\n")
	m := core.Message{
		Conversation: "respondio:1",
		Content:      []core.ContentBlock{{Type: "image", Data: "AAAA", MimeType: "image/png"}},
	}

	if err := h.Record(context.Background(), m, core.RoleUser); err == nil {
		t.Fatal("Record succeeded, want an error about the missing text")
	}
}

// emitted runs one turn and answers every emit call it made, so a test states
// the streamed reply as literals.
func emitted(t *testing.T, h *Harness, conversation string) [][]core.ContentBlock {
	t.Helper()
	var calls [][]core.ContentBlock
	m := core.Message{Conversation: conversation, Content: core.Text("hi")}
	if err := h.Turn(context.Background(), m, func(content []core.ContentBlock) error {
		calls = append(calls, content)
		return nil
	}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	return calls
}

// turn runs one turn and drops what it emitted, for a test that asserts
// something else about it.
func turn(t *testing.T, h *Harness, conversation string) {
	t.Helper()
	emitted(t, h, conversation)
}

// TestEachChunkIsEmittedAsItArrives pins that the reply reaches the channel while
// the turn runs: the stub agent streams two chunks, and both are emitted before
// Turn returns.
func TestEachChunkIsEmittedAsItArrives(t *testing.T) {
	h, _ := harness(t)
	calls := emitted(t, h, "respondio:1")
	want := []core.ContentBlock{
		{Type: "text", Text: "hello from\n\n"},
		{Type: "text", Text: "the stub agent"},
	}
	if len(calls) != len(want) {
		t.Fatalf("emit was called %d times with %+v, want %d calls", len(calls), calls, len(want))
	}
	for i, content := range calls {
		if len(content) != 1 || content[0] != want[i] {
			t.Fatalf("emit call %d carried %+v, want %+v", i, content, want[i])
		}
	}
}

// TestAChunkAfterTheTurnIsNotEmitted pins what momo does with content the agent
// streams once it has answered the prompt: the turn is over, so the chunk reaches
// no channel. Under the race detector it holds the turn and the connection apart
// as well.
func TestAChunkAfterTheTurnIsNotEmitted(t *testing.T) {
	t.Setenv("STUBAGENT_LATE_CHUNK", "1")
	h, _ := harness(t)
	for i, content := range emitted(t, h, "respondio:1") {
		if content[0].Text == "after the turn" {
			t.Fatalf("emit call %d carried the chunk the agent streamed after the turn", i)
		}
	}
}

func TestTheSubprocessIsGoneAfterTheTurn(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("STUBAGENT_PID_FILE", pidFile)
	h, _ := harness(t)
	turn(t, h, "respondio:1")
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
	turn(t, h, "respondio:1")
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
			m := core.Message{Conversation: conversation, Content: core.Text("hi")}
			failed <- h.Turn(context.Background(), m, func([]core.ContentBlock) error { return nil })
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
	turn(t, h, "respondio:1")
	cwd, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatalf("the stub agent reported no cwd: %v", err)
	}
	if !filepath.IsAbs(string(cwd)) {
		t.Fatalf("session/new cwd = %q, want an absolute path", cwd)
	}
}

// sessionTrace runs one turn for each conversation, one after the other, and
// answers the lines the stub agent wrote and the data directory they name, so a
// test states the whole session traffic as literals.
func sessionTrace(t *testing.T, conversations ...string) ([]string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace")
	t.Setenv("STUBAGENT_TRACE", path)
	h, root := harness(t)
	for _, conversation := range conversations {
		turn(t, h, conversation)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the stub agent wrote no trace: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n"), root
}

func wantTrace(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("trace =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestASecondMessageResumesTheSessionOfTheFirst holds momo to one session for
// each conversation: the second turn lists and resumes, and creates nothing.
func TestASecondMessageResumesTheSessionOfTheFirst(t *testing.T) {
	got, root := sessionTrace(t, "respondio:1", "respondio:1")
	dir := filepath.Join(root, dirName("respondio:1"))
	wantTrace(t, got, []string{
		"session/list\t" + dir,
		"session/new\t" + dir + "\tstub-session-0",
		"session/list\t" + dir,
		"session/resume\tstub-session-0",
	})
}

func TestTwoConversationsGetTwoSessions(t *testing.T) {
	got, root := sessionTrace(t, "respondio:1", "respondio:2")
	one := filepath.Join(root, dirName("respondio:1"))
	two := filepath.Join(root, dirName("respondio:2"))
	wantTrace(t, got, []string{
		"session/list\t" + one,
		"session/new\t" + one + "\tstub-session-0",
		"session/list\t" + two,
		"session/new\t" + two + "\tstub-session-1",
	})
}

// TestAnAgentWithoutSessionCapabilitiesGetsANewSession pins what momo does with
// an agent it cannot list: momo keeps no session id of its own, so every turn
// starts a session.
func TestAnAgentWithoutSessionCapabilitiesGetsANewSession(t *testing.T) {
	t.Setenv("STUBAGENT_NO_SESSION_CAPS", "1")
	got, root := sessionTrace(t, "respondio:1", "respondio:1")
	dir := filepath.Join(root, dirName("respondio:1"))
	wantTrace(t, got, []string{
		"session/new\t" + dir + "\tstub-session-0",
		"session/new\t" + dir + "\tstub-session-1",
	})
}

// TestATurnAnswersThePermissionRequestAndStillReplies pins what momo does with a
// request it can answer nobody with: it selects an option that allows the action,
// and the turn produces its reply. The stub offers a refusing option first, so
// selecting the first option of the list is not enough to pass.
func TestATurnAnswersThePermissionRequestAndStillReplies(t *testing.T) {
	for name, offered := range map[string]struct{ options, want string }{
		"an allowing option": {options: "all", want: "session/request_permission\tselected\tallow-once"},
		"only refusals":      {options: "refusals", want: "session/request_permission\tcancelled\t"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("STUBAGENT_PERMISSION", offered.options)
			path := filepath.Join(t.TempDir(), "trace")
			t.Setenv("STUBAGENT_TRACE", path)
			h, _ := harness(t)
			calls := emitted(t, h, "respondio:1")
			if len(calls) != 2 || calls[0][0].Text != "hello from\n\n" {
				t.Fatalf("emitted %+v, want the reply of the stub agent", calls)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("the stub agent wrote no trace: %v", err)
			}
			if !strings.Contains(string(raw), offered.want) {
				t.Fatalf("trace =\n%s\nwant a line %q", raw, offered.want)
			}
		})
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
