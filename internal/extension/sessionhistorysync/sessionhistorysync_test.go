package sessionhistorysync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/core"
)

const commands = "record_user_message_command: \"/store-user\"\n" +
	"record_assistant_message_command: \"/store-assistant\"\n"

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

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fake keeps the prompts it gets and streams the chunks it was given, so a test
// needs no ACP process.
type fake struct {
	conversations []string
	prompts       [][]core.ContentBlock
	chunks        []string
}

func (f *fake) Prompt(_ context.Context, conversation string, prompt []core.ContentBlock, emit core.Emit) error {
	f.conversations = append(f.conversations, conversation)
	f.prompts = append(f.prompts, prompt)
	for _, chunk := range f.chunks {
		if err := emit(core.Text(chunk)); err != nil {
			return err
		}
	}
	return nil
}

func record(t *testing.T, log *slog.Logger, p Prompter) Recorder {
	t.Helper()
	r, err := New(log, decoder(commands), p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestEachRoleGetsItsOwnCommandOnOneLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		role Role
		want string
	}{
		{name: "user", role: RoleUser, want: "/store-user hello there"},
		{name: "assistant", role: RoleAssistant, want: "/store-assistant hello there"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &fake{}
			m := core.Message{Conversation: "respondio:123", Content: core.Text("hello\nthere")}
			if err := record(t, discard(), p).Record(context.Background(), m, tc.role); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if len(p.prompts) != 1 {
				t.Fatalf("the prompter was called %d times, want 1", len(p.prompts))
			}
			want := core.Text(tc.want)
			if len(p.prompts[0]) != 1 || p.prompts[0][0] != want[0] {
				t.Fatalf("prompt = %+v, want %+v", p.prompts[0], want)
			}
		})
	}
}

func TestTheQualifiedConversationReachesThePrompter(t *testing.T) {
	p := &fake{}
	m := core.Message{Conversation: "respondio:123", Content: core.Text("hello")}
	if err := record(t, discard(), p).Record(context.Background(), m, RoleUser); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(p.conversations) != 1 || p.conversations[0] != "respondio:123" {
		t.Fatalf("conversations = %v, want [respondio:123]", p.conversations)
	}
}

// TestStreamedOutputIsDiscardedIntoTheDebugLog pins the only diagnosis this path
// has: the answer of the agent reaches the debug log and nothing else.
func TestStreamedOutputIsDiscardedIntoTheDebugLog(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := &fake{chunks: []string{"unknown command"}}
	m := core.Message{Conversation: "respondio:123", Content: core.Text("hello")}
	if err := record(t, log, p).Record(context.Background(), m, RoleUser); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !strings.Contains(out.String(), "unknown command") || !strings.Contains(out.String(), "respondio:123") {
		t.Fatalf("log =\n%s\nwant the discarded text and the conversation", out.String())
	}
}

func TestRecordRefusesWhatItCannotSend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []core.ContentBlock
		role    Role
	}{
		{name: "an image only", content: []core.ContentBlock{{Type: "image", Data: "aGk=", MimeType: "image/png"}}, role: RoleUser},
		{name: "no content", content: nil, role: RoleUser},
		{name: "an unknown role", content: core.Text("hello"), role: Role("operator")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &fake{}
			m := core.Message{Conversation: "respondio:123", Content: tc.content}
			if err := record(t, discard(), p).Record(context.Background(), m, tc.role); err == nil {
				t.Fatal("Record succeeded, want an error")
			}
			if len(p.prompts) != 0 {
				t.Fatalf("the prompter was called with %+v, want no call", p.prompts)
			}
		})
	}
}

func TestNewRefusesAnEnabledExtensionWithoutItsCommands(t *testing.T) {
	for name, body := range map[string]string{
		"no command at all":      "",
		"no user command":        "record_assistant_message_command: \"/store-assistant\"\n",
		"no assistant command":   "record_user_message_command: \"/store-user\"\n",
		"an unknown setting":     commands + "record_operator_message_command: \"/store-operator\"\n",
		"an empty user command":  "record_user_message_command: \"\"\nrecord_assistant_message_command: \"/a\"\n",
		"an empty other command": "record_user_message_command: \"/a\"\nrecord_assistant_message_command: \"\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(discard(), decoder(body), &fake{}); err == nil {
				t.Fatal("New succeeded, want an error naming the missing command")
			}
		})
	}
}

func TestQualifyingPrefixesTheChannelName(t *testing.T) {
	p := &fake{}
	base := record(t, discard(), p)
	m := core.Message{Conversation: "123", Content: core.Text("hello")}
	if err := Qualifying("respondio", base).Record(context.Background(), m, RoleUser); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(p.conversations) != 1 || p.conversations[0] != "respondio:123" {
		t.Fatalf("conversations = %v, want [respondio:123]", p.conversations)
	}
}

func TestQualifyingADisabledExtensionStaysNil(t *testing.T) {
	if got := Qualifying("respondio", nil); got != nil {
		t.Fatalf("Qualifying(nil) = %+v, want nil", got)
	}
}
