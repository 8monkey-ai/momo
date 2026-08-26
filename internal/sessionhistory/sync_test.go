package sessionhistory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/core"
)

// Sync records through the same abstraction a channel asks for, so a channel
// never depends on this package.
var _ core.History = (*Sync)(nil)

func TestTheConfigurationKeyIsFixed(t *testing.T) {
	if Name != "session-history-sync" {
		t.Fatalf("Name = %q, want \"session-history-sync\"", Name)
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// decoder decodes a YAML block strictly, the way config hands one over.
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

const commands = "user_message_command: /momo-user\nassistant_message_command: /momo-assistant\n"

type prompt struct {
	conversation string
	text         string
}

type agent struct {
	answer string
	err    error

	mu      sync.Mutex
	prompts []prompt
}

func (a *agent) Turn(_ context.Context, m core.Message, emit core.Emit) error {
	a.mu.Lock()
	a.prompts = append(a.prompts, prompt{conversation: m.Conversation, text: core.TextOf(m.Content)})
	a.mu.Unlock()
	if a.answer != "" {
		if err := emit(core.Text(a.answer)); err != nil {
			return err
		}
	}
	return a.err
}

func (a *agent) only(t *testing.T) prompt {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.prompts) != 1 {
		t.Fatalf("the agent got %d prompts (%+v), want one", len(a.prompts), a.prompts)
	}
	return a.prompts[0]
}

func newSync(t *testing.T, a core.Agent, log *slog.Logger) *Sync {
	t.Helper()
	s, err := New(log, decoder(commands), a)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func message(text string) core.Message {
	return core.Message{Conversation: "respondio:12345", Content: core.Text(text)}
}

func TestARecordedUserMessageIsOneSlashCommandPrompt(t *testing.T) {
	a := &agent{}
	newSync(t, a, discard()).RecordUser(context.Background(), message("hello there"))
	got := a.only(t)
	want := prompt{conversation: "respondio:12345", text: "/momo-user hello there"}
	if got != want {
		t.Fatalf("prompt = %+v, want %+v", got, want)
	}
}

func TestARecordedAssistantMessageUsesTheAssistantCommand(t *testing.T) {
	a := &agent{}
	newSync(t, a, discard()).RecordAssistant(context.Background(), message("on my way"))
	got := a.only(t)
	want := prompt{conversation: "respondio:12345", text: "/momo-assistant on my way"}
	if got != want {
		t.Fatalf("prompt = %+v, want %+v", got, want)
	}
}

// A slash command ends at the first line break.
func TestLineBreaksBecomeSpaces(t *testing.T) {
	for name, tc := range map[string]struct{ text, want string }{
		"line feed":                 {text: "first\nsecond", want: "/momo-user first second"},
		"carriage return":           {text: "first\rsecond", want: "/momo-user first second"},
		"carriage return line feed": {text: "first\r\nsecond", want: "/momo-user first second"},
		"a blank line":              {text: "first\n\nsecond", want: "/momo-user first  second"},
	} {
		t.Run(name, func(t *testing.T) {
			a := &agent{}
			newSync(t, a, discard()).RecordUser(context.Background(), message(tc.text))
			if got := a.only(t).text; got != tc.want {
				t.Fatalf("prompt text = %q, want %q", got, tc.want)
			}
		})
	}
}

// A base64 payload on the command line is not text the agent can read back.
func TestOnlyTextIsRecorded(t *testing.T) {
	a := &agent{}
	m := core.Message{Conversation: "respondio:12345", Content: []core.ContentBlock{
		{Type: "image", Data: "AAAA", MimeType: "image/png"},
		{Type: "text", Text: "look"},
	}}
	newSync(t, a, discard()).RecordUser(context.Background(), m)
	if got := a.only(t).text; got != "/momo-user look" {
		t.Fatalf("prompt text = %q, want \"/momo-user look\"", got)
	}
}

func logged(t *testing.T, a core.Agent) string {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	newSync(t, a, log).RecordUser(context.Background(), message("hello there"))
	return buf.String()
}

// A failed record is the operator's to read, never the contact's.
func TestAFailedRecordIsLogged(t *testing.T) {
	got := logged(t, &agent{err: errors.New("the agent exited before it answered")})
	for _, want := range []string{"respondio:12345", "the agent exited before it answered"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log =\n%s\nwant it to carry %q", got, want)
		}
	}
}

// An agent that does not support the command refuses in its reply, which the
// operator reads instead of guessing at an empty session.
func TestTheAnswerToTheCommandIsLogged(t *testing.T) {
	got := logged(t, &agent{answer: "Unknown command: /momo-user"})
	if !strings.Contains(got, "Unknown command: /momo-user") {
		t.Fatalf("log =\n%s\nwant it to carry the agent's answer", got)
	}
}

func TestNewRequiresBothCommands(t *testing.T) {
	for name, body := range map[string]string{
		"an empty block":             "",
		"only the user command":      "user_message_command: /momo-user\n",
		"only the assistant command": "assistant_message_command: /momo-assistant\n",
		"an empty user command":      "user_message_command: \"\"\nassistant_message_command: /momo-assistant\n",
		"a misspelled setting":       "user_mesage_command: /momo-user\nassistant_message_command: /momo-assistant\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(discard(), decoder(body), &agent{}); err == nil {
				t.Fatal("New succeeded, want an error naming what the block must hold")
			}
		})
	}
}

func TestNewAcceptsBothCommands(t *testing.T) {
	if _, err := New(discard(), decoder(commands), &agent{}); err != nil {
		t.Fatalf("New: %v", err)
	}
}
