package sessionhistory

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

type prompt struct {
	conversation string
	content      []core.ContentBlock
}

// fake is a Prompter that records the calls it got and streams what a test tells
// it to stream.
type fake struct {
	calls   []prompt
	streams []core.ContentBlock
}

func (f *fake) Prompt(_ context.Context, conversation string, content []core.ContentBlock, emit core.Emit) error {
	f.calls = append(f.calls, prompt{conversation: conversation, content: content})
	for _, block := range f.streams {
		if err := emit([]core.ContentBlock{block}); err != nil {
			return err
		}
	}
	return nil
}

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

const commands = "record_user_message_command: \"/store-user\"\n" +
	"record_assistant_message_command: \"/store-assistant\"\n"

func recorderWith(t *testing.T, log *slog.Logger, p Prompter) Recorder {
	t.Helper()
	r, err := New(log, decoder(commands), p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestTheRoleSelectsTheConfiguredCommand(t *testing.T) {
	for name, tc := range map[string]struct {
		role Role
		want string
	}{
		"user":      {role: RoleUser, want: "/store-user hello there"},
		"assistant": {role: RoleAssistant, want: "/store-assistant hello there"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &fake{}
			r := recorderWith(t, discard(), p)

			m := core.Message{Conversation: "respondio:123", Content: core.Text("hello\nthere")}
			if err := r.Record(context.Background(), m, tc.role); err != nil {
				t.Fatalf("Record: %v", err)
			}
			if len(p.calls) != 1 {
				t.Fatalf("the prompter got %d calls, want 1", len(p.calls))
			}
			want := []core.ContentBlock{{Type: "text", Text: tc.want}}
			if len(p.calls[0].content) != 1 || p.calls[0].content[0] != want[0] {
				t.Fatalf("prompt = %+v, want %+v", p.calls[0].content, want)
			}
		})
	}
}

func TestTheQualifiedConversationReachesThePrompter(t *testing.T) {
	p := &fake{}
	r := recorderWith(t, discard(), p)

	m := core.Message{Conversation: "respondio:123", Content: core.Text("hi")}
	if err := r.Record(context.Background(), m, RoleUser); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(p.calls) != 1 || p.calls[0].conversation != "respondio:123" {
		t.Fatalf("prompter calls = %+v, want the conversation \"respondio:123\"", p.calls)
	}
}

func TestTheStreamedReplyIsDiscardedIntoTheDebugLog(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := &fake{streams: core.Text("unknown command")}
	r := recorderWith(t, log, p)

	m := core.Message{Conversation: "respondio:123", Content: core.Text("hi")}
	if err := r.Record(context.Background(), m, RoleUser); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "unknown command") || !strings.Contains(got, "respondio:123") {
		t.Fatalf("log =\n%s\nwant the discarded text and the conversation", got)
	}
}

func TestRecordRefusesWhatItCannotSend(t *testing.T) {
	for name, tc := range map[string]struct {
		message core.Message
		role    Role
	}{
		"an image only":   {message: core.Message{Conversation: "respondio:1", Content: []core.ContentBlock{{Type: "image", Data: "AAAA", MimeType: "image/png"}}}, role: RoleUser},
		"an unknown role": {message: core.Message{Conversation: "respondio:1", Content: core.Text("hi")}, role: Role("operator")},
	} {
		t.Run(name, func(t *testing.T) {
			p := &fake{}
			r := recorderWith(t, discard(), p)

			if err := r.Record(context.Background(), tc.message, tc.role); err == nil {
				t.Fatal("Record succeeded, want an error")
			}
			if len(p.calls) != 0 {
				t.Fatalf("the prompter got %+v, want no call", p.calls)
			}
		})
	}
}

func TestNewRefusesAnEnabledExtensionWithoutItsCommands(t *testing.T) {
	for name, body := range map[string]string{
		"no commands":      "",
		"no user command":  "record_assistant_message_command: \"/store-assistant\"\n",
		"no assistant":     "record_user_message_command: \"/store-user\"\n",
		"unknown setting":  commands + "record_operator_message_command: \"/store-operator\"\n",
		"empty user value": "record_user_message_command: \"\"\nrecord_assistant_message_command: \"/store-assistant\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(discard(), decoder(body), &fake{}); err == nil {
				t.Fatal("New succeeded, want an error naming the missing command")
			}
		})
	}
}

func TestQualifiedPrefixesTheConversationWithTheChannelName(t *testing.T) {
	p := &fake{}
	r := Qualified("respondio", recorderWith(t, discard(), p))

	m := core.Message{Conversation: "123", Content: core.Text("hi")}
	if err := r.Record(context.Background(), m, RoleUser); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(p.calls) != 1 || p.calls[0].conversation != "respondio:123" {
		t.Fatalf("prompter calls = %+v, want the conversation \"respondio:123\"", p.calls)
	}
}

func TestQualifiedKeepsAnAbsentRecorderAbsent(t *testing.T) {
	if got := Qualified("respondio", nil); got != nil {
		t.Fatalf("Qualified wrapped a nil recorder into %+v, want nil", got)
	}
}
