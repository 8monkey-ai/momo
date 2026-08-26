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

// fakeAgent answers a record with the output it was given, and keeps every
// prompt.
type fakeAgent struct {
	mu      sync.Mutex
	prompts []core.Message
	output  []string
	err     error
}

func (f *fakeAgent) Turn(_ context.Context, m core.Message, emit core.Emit) error {
	f.mu.Lock()
	f.prompts = append(f.prompts, m)
	output := f.output
	f.mu.Unlock()
	for _, text := range output {
		if err := emit(core.Text(text)); err != nil {
			return err
		}
	}
	return f.err
}

func (f *fakeAgent) prompt(t *testing.T) core.Message {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 1 {
		t.Fatalf("the agent got %d prompts, want 1: %+v", len(f.prompts), f.prompts)
	}
	return f.prompts[0]
}

func (f *fakeAgent) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
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

const commands = "user_command: /user-message\nassistant_command: /assistant-message\n"

func recorder(t *testing.T, a core.Agent) (core.Handler, *bytes.Buffer) {
	t.Helper()
	var log bytes.Buffer
	h, err := New(slog.New(slog.NewTextHandler(&log, nil)), decoder(commands), a)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h == nil {
		t.Fatal("New answered no handler, want the configured recorder")
	}
	return h, &log
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func message(text string) core.Message {
	return core.Message{Conversation: "respondio:12345", Content: core.Text(text)}
}

// noReply fails the test if a record answers anybody.
func noReply(t *testing.T) core.Reply {
	t.Helper()
	return func(context.Context, []core.ContentBlock) error {
		t.Error("a recorded message was answered, want no reply")
		return nil
	}
}

func TestAConfigurationWithoutTheBlockLeavesTheExtensionOut(t *testing.T) {
	h, err := New(discard(), nil, &fakeAgent{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h != nil {
		t.Fatalf("New answered %+v, want no handler", h)
	}
}

func TestNewRequiresBothCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "nothing configured", body: ""},
		{name: "only the user command", body: "user_command: /user-message\n"},
		{name: "only the assistant command", body: "assistant_command: /assistant-message\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(discard(), decoder(tc.body), &fakeAgent{}); err == nil {
				t.Fatal("New succeeded, want an error naming the missing commands")
			}
		})
	}
}

func TestNewReportsAnUnknownSetting(t *testing.T) {
	if _, err := New(discard(), decoder("user_comand: /user-message\n"), &fakeAgent{}); err == nil {
		t.Fatal("New succeeded, want an error naming the unknown setting")
	}
}

func TestAnIncomingMessageIsRecordedWithTheUserCommand(t *testing.T) {
	a := &fakeAgent{}
	h, _ := recorder(t, a)
	if err := h.Received(context.Background(), message("hello"), noReply(t)); err != nil {
		t.Fatalf("Received: %v", err)
	}
	got := a.prompt(t)
	if got.Conversation != "respondio:12345" {
		t.Errorf("conversation = %q, want \"respondio:12345\"", got.Conversation)
	}
	if text := core.TextOf(got.Content); text != "/user-message hello" {
		t.Errorf("prompt = %q, want \"/user-message hello\"", text)
	}
}

func TestAnOutgoingMessageIsRecordedWithTheAssistantCommand(t *testing.T) {
	a := &fakeAgent{}
	h, _ := recorder(t, a)
	h.Sent(context.Background(), message("on my way"))
	got := a.prompt(t)
	if got.Conversation != "respondio:12345" {
		t.Errorf("conversation = %q, want \"respondio:12345\"", got.Conversation)
	}
	if text := core.TextOf(got.Content); text != "/assistant-message on my way" {
		t.Errorf("prompt = %q, want \"/assistant-message on my way\"", text)
	}
}

// TestALineBreakBecomesASpace pins that the command stays on one line: an agent
// reads the second line of a prompt as text and not as part of the command.
func TestALineBreakBecomesASpace(t *testing.T) {
	a := &fakeAgent{}
	h, _ := recorder(t, a)
	h.Sent(context.Background(), message("first\nsecond\r\nthird\rfourth"))
	if text := core.TextOf(a.prompt(t).Content); text != "/assistant-message first second third fourth" {
		t.Errorf("prompt = %q, want \"/assistant-message first second third fourth\"", text)
	}
}

func TestAMessageWithNoTextIsNotRecorded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []core.ContentBlock
	}{
		{name: "no blocks"},
		{name: "empty text", content: core.Text("")},
		{name: "whitespace only", content: core.Text(" \n ")},
		{name: "an image", content: []core.ContentBlock{{Type: "image", Data: "AAAA", MimeType: "image/png"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAgent{}
			h, _ := recorder(t, a)
			h.Sent(context.Background(), core.Message{Conversation: "respondio:1", Content: tc.content})
			if a.calls() != 0 {
				t.Fatalf("the agent got %d prompts, want none", a.calls())
			}
		})
	}
}

func TestAFailedRecordIsLoggedAndNotReported(t *testing.T) {
	a := &fakeAgent{err: errors.New("the agent has no such command")}
	h, log := recorder(t, a)
	if err := h.Received(context.Background(), message("hello"), noReply(t)); err != nil {
		t.Fatalf("Received reported %v, want no error: a failed record is not the channel's to report", err)
	}
	if !strings.Contains(log.String(), "the agent has no such command") {
		t.Fatalf("log = %q, want the failure of the record", log.String())
	}
}

// TestTheAgentOutputOfARecordIsLogged pins the diagnosis an operator gets when
// the configured command is not the one the agent serves.
func TestTheAgentOutputOfARecordIsLogged(t *testing.T) {
	a := &fakeAgent{output: []string{"Unknown command:", "/user-message"}}
	h, log := recorder(t, a)
	if err := h.Received(context.Background(), message("hello"), noReply(t)); err != nil {
		t.Fatalf("Received: %v", err)
	}
	for _, want := range []string{"Unknown command:", "/user-message"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log = %q, want it to hold %q", log.String(), want)
		}
	}
}
