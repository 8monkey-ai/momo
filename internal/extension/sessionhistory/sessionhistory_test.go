package sessionhistory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/8monkey-ai/momo/internal/core"
)

type prompts struct {
	got   []core.Message
	reply string
	err   error
}

func (p *prompts) Turn(_ context.Context, m core.Message, emit core.Emit) error {
	p.got = append(p.got, m)
	if p.reply != "" {
		if err := emit(core.Text(p.reply)); err != nil {
			return err
		}
	}
	return p.err
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func block(user, assistant string) func(any) error {
	return func(v any) error {
		s, ok := v.(*settings)
		if !ok {
			return errors.New("decoded into the wrong type")
		}
		s.UserCommand = user
		s.AssistantCommand = assistant
		return nil
	}
}

func absent(any) error { return nil }

func sync(t *testing.T, a core.Agent) core.Recorder {
	t.Helper()
	r, err := New(discard(), block("/history-user", "/history-assistant"), a)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestNoBlockRecordsNothing(t *testing.T) {
	a := &prompts{}
	r, err := New(discard(), absent, a)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.Enabled() {
		t.Fatal("Enabled() = true, want false without the session_history block")
	}
	if err := r.RecordUser(context.Background(), core.Message{Conversation: "respondio:1", Content: core.Text("hello")}); err != nil {
		t.Fatalf("RecordUser: %v", err)
	}
	if err := r.RecordAssistant(context.Background(), core.Message{Conversation: "respondio:1", Content: core.Text("hello")}); err != nil {
		t.Fatalf("RecordAssistant: %v", err)
	}
	if len(a.got) != 0 {
		t.Fatalf("the agent got %+v, want no prompt", a.got)
	}
}

func TestAConfiguredBlockRecords(t *testing.T) {
	if !sync(t, &prompts{}).Enabled() {
		t.Fatal("Enabled() = false, want true with both commands configured")
	}
}

func TestNewRefusesCommandsItCannotSend(t *testing.T) {
	for name, tc := range map[string]struct{ user, assistant string }{
		"only the user command":      {user: "/history-user"},
		"only the assistant command": {assistant: "/history-assistant"},
		"one command for both roles": {user: "/history", assistant: "/history"},
		"not a slash command":        {user: "history-user", assistant: "/history-assistant"},
		"a command with a space":     {user: "/history user", assistant: "/history-assistant"},
		"a command with a line break": {
			user:      "/history-user\n",
			assistant: "/history-assistant",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(discard(), block(tc.user, tc.assistant), &prompts{}); err == nil {
				t.Fatal("New succeeded, want an error naming the command")
			}
		})
	}
}

func TestARecordIsOneSlashCommandPrompt(t *testing.T) {
	for name, tc := range map[string]struct {
		record func(core.Recorder, core.Message) error
		want   string
	}{
		"a user message": {
			record: func(r core.Recorder, m core.Message) error { return r.RecordUser(context.Background(), m) },
			want:   "/history-user hello",
		},
		"an assistant message": {
			record: func(r core.Recorder, m core.Message) error { return r.RecordAssistant(context.Background(), m) },
			want:   "/history-assistant hello",
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := &prompts{}
			m := core.Message{Conversation: "respondio:123", Content: core.Text("hello")}
			if err := tc.record(sync(t, a), m); err != nil {
				t.Fatalf("record: %v", err)
			}
			want := []core.Message{{Conversation: "respondio:123", Content: core.Text(tc.want)}}
			if !reflect.DeepEqual(a.got, want) {
				t.Fatalf("the agent got %+v, want %+v", a.got, want)
			}
		})
	}
}

func TestLineBreaksBecomeSpacesSoTheCommandStaysOnOneLine(t *testing.T) {
	a := &prompts{}
	m := core.Message{Conversation: "respondio:1", Content: core.Text("  first\nsecond\r\nthird\rfourth  ")}
	if err := sync(t, a).RecordUser(context.Background(), m); err != nil {
		t.Fatalf("RecordUser: %v", err)
	}
	got := core.TextOf(a.got[0].Content)
	if got != "/history-user first second third fourth" {
		t.Fatalf("prompt = %q, want \"/history-user first second third fourth\"", got)
	}
}

func TestAMessageWithNoTextIsNotRecorded(t *testing.T) {
	a := &prompts{}
	m := core.Message{Conversation: "respondio:1", Content: []core.ContentBlock{{Type: "image", Data: "AAAA", MimeType: "image/png"}}}
	if err := sync(t, a).RecordUser(context.Background(), m); err != nil {
		t.Fatalf("RecordUser: %v", err)
	}
	if len(a.got) != 0 {
		t.Fatalf("the agent got %+v, want no prompt", a.got)
	}
}

func TestAFailedRecordIsReportedAndLogged(t *testing.T) {
	var log bytes.Buffer
	broken := errors.New("the agent exited before it answered")
	r, err := New(slog.New(slog.NewTextHandler(&log, nil)), block("/history-user", "/history-assistant"), &prompts{err: broken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := core.Message{Conversation: "respondio:123", Content: core.Text("hello")}
	if err := r.RecordUser(context.Background(), m); !errors.Is(err, broken) {
		t.Fatalf("RecordUser = %v, want it to wrap %v", err, broken)
	}
	if !strings.Contains(log.String(), "respondio:123") || !strings.Contains(log.String(), broken.Error()) {
		t.Fatalf("log = %q, want the conversation and the reason", log.String())
	}
}

// TestTheAnswerToARecordIsLogged pins the only sign momo gets of an agent that
// does not support the command: the text it answers with.
func TestTheAnswerToARecordIsLogged(t *testing.T) {
	var log bytes.Buffer
	r, err := New(slog.New(slog.NewTextHandler(&log, nil)), block("/history-user", "/history-assistant"), &prompts{reply: "Unknown command: /history-user"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := core.Message{Conversation: "respondio:123", Content: core.Text("hello")}
	if err := r.RecordUser(context.Background(), m); err != nil {
		t.Fatalf("RecordUser: %v", err)
	}
	if !strings.Contains(log.String(), "Unknown command: /history-user") {
		t.Fatalf("log = %q, want the answer the agent gave", log.String())
	}
}
