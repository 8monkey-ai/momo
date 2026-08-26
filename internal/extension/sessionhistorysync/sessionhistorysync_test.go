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

const commands = "user_message_command: \"/user-message\"\nassistant_message_command: \"/assistant-message\"\n"

// agent answers a record turn with the text it was given and remembers the
// prompts it received.
type agent struct {
	prompts []core.Message
	answer  []string
	err     error
}

func (a *agent) Turn(_ context.Context, m core.Message, emit core.Emit) error {
	a.prompts = append(a.prompts, m)
	for _, text := range a.answer {
		if err := emit(core.Text(text)); err != nil {
			return err
		}
	}
	return a.err
}

func history(t *testing.T, log *slog.Logger, a core.Agent) core.History {
	t.Helper()
	h, err := New(log, decoder(commands), a)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h == nil {
		t.Fatal("New answered no history, want one for a configured block")
	}
	return h
}

func TestWithoutTheBlockNothingIsRecorded(t *testing.T) {
	h, err := New(discard(), nil, &agent{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h != nil {
		t.Fatalf("New answered %v, want no history for an absent block", h)
	}
}

func TestBothCommandsAreRequired(t *testing.T) {
	for name, body := range map[string]string{
		"an empty block":             "",
		"only the user command":      "user_message_command: \"/user-message\"\n",
		"only the assistant command": "assistant_message_command: \"/assistant-message\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(discard(), decoder(body), &agent{}); err == nil {
				t.Fatal("New succeeded, want an error naming the missing command")
			}
		})
	}
}

func TestARecordIsOnePromptTurnOfTheConversation(t *testing.T) {
	for name, tc := range map[string]struct {
		record func(core.History, core.Message)
		want   string
	}{
		"a user message": {
			record: func(h core.History, m core.Message) { h.RecordUser(context.Background(), m) },
			want:   "/user-message hello",
		},
		"an assistant message": {
			record: func(h core.History, m core.Message) { h.RecordAssistant(context.Background(), m) },
			want:   "/assistant-message hello",
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := &agent{}
			tc.record(history(t, discard(), a), core.Message{Conversation: "respondio:1", Content: core.Text("hello")})
			if len(a.prompts) != 1 {
				t.Fatalf("the agent got %d prompts, want 1", len(a.prompts))
			}
			got := a.prompts[0]
			if got.Conversation != "respondio:1" {
				t.Errorf("conversation = %q, want \"respondio:1\"", got.Conversation)
			}
			if len(got.Content) != 1 || got.Content[0] != (core.ContentBlock{Type: "text", Text: tc.want}) {
				t.Errorf("prompt = %+v, want the single text block %q", got.Content, tc.want)
			}
		})
	}
}

func TestALineBreakBecomesASpace(t *testing.T) {
	a := &agent{}
	m := core.Message{Conversation: "respondio:1", Content: core.Text("first\nsecond\r\nthird\rfourth")}
	history(t, discard(), a).RecordUser(context.Background(), m)
	if got := core.TextOf(a.prompts[0].Content); got != "/user-message first second third fourth" {
		t.Fatalf("prompt = %q, want every line break as one space", got)
	}
}

func TestOnlyTextIsRecorded(t *testing.T) {
	a := &agent{}
	m := core.Message{Conversation: "respondio:1", Content: []core.ContentBlock{
		{Type: "image", Data: "aGVsbG8=", MimeType: "image/png"},
		{Type: "text", Text: "look at this"},
	}}
	history(t, discard(), a).RecordUser(context.Background(), m)
	if got := core.TextOf(a.prompts[0].Content); got != "/user-message look at this" {
		t.Fatalf("prompt = %q, want the text of the message only", got)
	}
}

// logged answers what momo wrote to its log while it recorded one message.
func logged(t *testing.T, a core.Agent) string {
	t.Helper()
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	m := core.Message{Conversation: "respondio:1", Content: core.Text("hello")}
	history(t, log, a).RecordUser(context.Background(), m)
	return out.String()
}

func TestAFailedRecordIsLogged(t *testing.T) {
	got := logged(t, &agent{err: errors.New("the agent exited before it answered")})
	for _, want := range []string{"respondio:1", "the agent exited before it answered"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log =\n%s\nwant a record naming %q", got, want)
		}
	}
}

// TestTheAnswerToARecordIsLogged pins the diagnosis of an agent that does not
// support the command: momo verifies no command, so what the agent answered is
// the only sign the record did not land.
func TestTheAnswerToARecordIsLogged(t *testing.T) {
	got := logged(t, &agent{answer: []string{"unknown command: ", "/user-message"}})
	for _, want := range []string{"respondio:1", "unknown command:  /user-message"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log =\n%s\nwant a record naming %q", got, want)
		}
	}
}

func TestARecordThatTheAgentAnsweredNothingToIsNotReportedAsAFailure(t *testing.T) {
	if got := logged(t, &agent{}); strings.Contains(got, "level=ERROR") {
		t.Fatalf("log =\n%s\nwant no error for a record the agent took", got)
	}
}
