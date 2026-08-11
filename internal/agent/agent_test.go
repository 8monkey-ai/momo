package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/channel"
	_ "github.com/8monkey-ai/momo/internal/channel/respondio"
	"github.com/8monkey-ai/momo/internal/core"
)

// stub is the path to the stub agent binary, built once for the whole package.
var stub string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "stubagent")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temporary directory: %v\n", err)
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

// newAgent builds an agent from a configuration block and reports the data root
// that block named, so a test can look at what a turn left on disk.
func newAgent(t *testing.T, block string) (*Agent, string) {
	t.Helper()
	decode := func(v any) error { return yaml.Unmarshal([]byte(block), v) }
	a, err := New(decode, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var s settings
	if err := yaml.Unmarshal([]byte(block), &s); err != nil {
		t.Fatalf("settings: %v", err)
	}
	return a, s.DataDir
}

func config(t *testing.T, args ...string) string {
	t.Helper()
	command := "[" + strconv.Quote(stub)
	for _, arg := range args {
		command += ", " + strconv.Quote(arg)
	}
	command += "]"
	return "command: " + command + "\ndata_dir: " + strconv.Quote(t.TempDir()) + "\n"
}

func turn(t *testing.T, a *Agent, conversation, text string) string {
	t.Helper()
	content, err := a.Turn(context.Background(), conversation, core.Text(text))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	return core.TextOf(content)
}

func TestRequiresAHarnessCommandAndADataDirectory(t *testing.T) {
	for name, block := range map[string]string{
		"nothing configured": "",
		"no command":         "data_dir: /tmp/momo-agent\n",
		"no data directory":  "command: [" + strconv.Quote(stub) + "]\n",
		"relative data directory": "command: [" + strconv.Quote(stub) + "]\n" +
			"data_dir: momo-agent\n",
		"unknown command": "command: [/nonexistent/harness]\ndata_dir: /tmp/momo-agent\n",
	} {
		t.Run(name, func(t *testing.T) {
			decode := func(v any) error { return yaml.Unmarshal([]byte(block), v) }
			if _, err := New(decode, slog.New(slog.DiscardHandler)); err == nil {
				t.Fatal("New succeeded, want the process to fail before it serves")
			}
		})
	}
}

func TestDeliversTheAgentsReplyOnTheChannelTheMessageArrivedOn(t *testing.T) {
	a, _ := newAgent(t, config(t))
	sent := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("reply body: %v", err)
		}
		sent <- payload.Message.Text
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	instances, err := channel.Build(context.Background(), map[string]channel.Decoder{
		"respondio": func(v any) error {
			return yaml.Unmarshal([]byte("received_secret: k\nsent_secret: k\napi_token: t\napi_url: "+api.URL+"\n"), v)
		},
	}, core.AgentHandler{Agent: a, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	webhook(t, instances[0], `{"event_type":"message.received","contact":{"id":77},`+
		`"message":{"message":{"type":"text","text":"hello"}}}`)

	select {
	case text := <-sent:
		if !strings.HasPrefix(text, "reply to hello session=") {
			t.Fatalf("reply = %q, want the stub agent's answer to \"hello\"", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no reply reached the channel")
	}
}

// webhook posts a signed respond.io event to the channel's first route.
func webhook(t *testing.T, instance channel.Instance, body string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, instance.Channel.Routes()[0].Path, strings.NewReader(body))
	r.Header.Set("X-Webhook-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	instance.Channel.Routes()[0].Handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200", w.Code)
	}
}

func TestSecondTurnResumesTheSessionTheFirstTurnCreated(t *testing.T) {
	a, _ := newAgent(t, config(t))

	first := turn(t, a, "respondio:1", "one")
	second := turn(t, a, "respondio:1", "two")
	other := turn(t, a, "acp:1", "three")

	if session(t, first) != session(t, second) {
		t.Fatalf("sessions = %q and %q, want the second turn to resume the first", first, second)
	}
	if session(t, other) == session(t, first) {
		t.Fatalf("a different conversation ran in session %q, want one of its own", session(t, other))
	}
}

func TestAResumedTurnCarriesOnlyItsOwnContent(t *testing.T) {
	a, _ := newAgent(t, config(t))

	turn(t, a, "respondio:2", "one")
	second := turn(t, a, "respondio:2", "two")

	if strings.Contains(second, "stale") {
		t.Fatalf("reply = %q, want only what this turn produced", second)
	}
	if !strings.Contains(second, "reply to two") {
		t.Fatalf("reply = %q, want the answer to this turn's prompt", second)
	}
}

func TestAnAgentThatNeitherListsNorResumesGetsANewSessionEveryTurn(t *testing.T) {
	a, _ := newAgent(t, config(t, "-sessions=false"))

	first := turn(t, a, "respondio:3", "one")
	second := turn(t, a, "respondio:3", "two")

	if session(t, first) == session(t, second) {
		t.Fatalf("both turns ran in session %q, want a new session for each", session(t, first))
	}
}

func TestPermissionRequestsAreApprovedWithAnOptionTheAgentOffered(t *testing.T) {
	a, _ := newAgent(t, config(t))

	if got := turn(t, a, "respondio:4", "act now"); !strings.Contains(got, "permission=yes") {
		t.Fatalf("reply = %q, want the allowing option the agent offered", got)
	}
}

func TestTheHarnessIsGoneWhenTheTurnEnds(t *testing.T) {
	a, root := newAgent(t, config(t))

	turn(t, a, "respondio:5", "one")

	pid := readPid(t, root)
	if err := syscall.Kill(pid, syscall.Signal(0)); err == nil {
		t.Fatalf("process %d is still running after the turn", pid)
	}
}

func TestAConversationIdentityBecomesOneDirectoryUnderTheDataRoot(t *testing.T) {
	a, root := newAgent(t, config(t))

	turn(t, a, "respondio:../../escaped/1", "one")

	names := conversations(t, root)
	if len(names) != 1 {
		t.Fatalf("data root holds %v, want one directory for the conversation", names)
	}
	if strings.ContainsAny(names[0], `/\.`) {
		t.Fatalf("directory %q carries a path separator or a dot", names[0])
	}
}

// conversations lists the directory names under the data root.
func conversations(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", root, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("data root holds the file %q, want only conversation directories", entry.Name())
		}
		names = append(names, entry.Name())
	}
	return names
}

func TestSeedsTheConversationDirectoryFromTheTemplateWithoutClobbering(t *testing.T) {
	template := t.TempDir()
	if err := os.WriteFile(filepath.Join(template, "AGENTS.md"), []byte("project rules"), 0o600); err != nil {
		t.Fatalf("template: %v", err)
	}
	a, root := newAgent(t, config(t)+"template: "+strconv.Quote(template)+"\n")

	turn(t, a, "respondio:6", "one")
	dir := filepath.Join(root, conversations(t, root)[0])
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("edited on the spot"), 0o600); err != nil {
		t.Fatalf("editing the seeded file: %v", err)
	}
	turn(t, a, "respondio:6", "two")

	seeded, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("reading the seeded file: %v", err)
	}
	if string(seeded) != "edited on the spot" {
		t.Fatalf("seeded file = %q, want the file already there left alone", seeded)
	}
}

// session reports the session id the stub agent ran the turn in.
func session(t *testing.T, reply string) string {
	t.Helper()
	_, id, found := strings.Cut(reply, "session=")
	if !found {
		t.Fatalf("reply = %q, want it to name the session it ran in", reply)
	}
	return id
}

// readPid reads the pid the stub agent wrote in the one conversation directory
// under the data root.
func readPid(t *testing.T, root string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, conversations(t, root)[0], "stubagent.pid"))
	if err != nil {
		t.Fatalf("reading the pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid %q: %v", raw, err)
	}
	return pid
}
