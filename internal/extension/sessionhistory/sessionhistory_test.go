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

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/core"
)

const commands = "user_message_command: /add-user-message\nassistant_message_command: /add-assistant-message\n"

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

type call struct {
	conversation string
	text         string
}

// handler stands in for the handler a channel's messages reach. It keeps what
// each record prompted, and answers with the text and the failure a test gives
// it.
type handler struct {
	calls []call
	emit  string
	err   error
	sent  int
}

func (h *handler) Received(ctx context.Context, m core.Message, reply core.Reply) error {
	h.calls = append(h.calls, call{conversation: m.Conversation, text: core.TextOf(m.Content)})
	if h.emit != "" {
		if err := reply(ctx, core.Text(h.emit)); err != nil {
			return err
		}
	}
	return h.err
}

func (h *handler) Sent(context.Context, core.Message) { h.sent++ }

func recording(t *testing.T, body string, h core.Handler) (channel.Recorder, *bytes.Buffer) {
	t.Helper()
	log := &bytes.Buffer{}
	s, err := New(slog.New(slog.NewTextHandler(log, nil)), decoder(body))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s == nil {
		t.Fatal("New answered no sync, want one for a configured block")
	}
	return s.Recorder(h), log
}

func message(text string) core.Message {
	return core.Message{Conversation: "respondio:12345", Content: core.Text(text)}
}

func TestAnAbsentBlockTurnsRecordingOff(t *testing.T) {
	s, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), decoder(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s != nil {
		t.Fatalf("New answered %+v, want nothing for an absent block", s)
	}
}

func TestBothCommandsAreRequired(t *testing.T) {
	for name, body := range map[string]string{
		"only the user command":      "user_message_command: /add-user-message\n",
		"only the assistant command": "assistant_message_command: /add-assistant-message\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), decoder(body)); err == nil {
				t.Fatal("New succeeded, want an error naming the missing command")
			}
		})
	}
}

// TestARecordIsOneSlashCommandLine pins the prompt a record sends: the command,
// the text behind it, and no line break in between, because an ACP agent reads a
// command and its argument on one line.
func TestARecordIsOneSlashCommandLine(t *testing.T) {
	h := &handler{}
	rec, _ := recording(t, commands, h)
	rec.RecordUser(context.Background(), message(" first line\nsecond line "))
	rec.RecordAssistant(context.Background(), message("windows\r\nline"))

	want := []call{
		{conversation: "respondio:12345", text: "/add-user-message  first line second line "},
		{conversation: "respondio:12345", text: "/add-assistant-message windows line"},
	}
	if !reflect.DeepEqual(h.calls, want) {
		t.Fatalf("the handler was prompted with %+v, want %+v", h.calls, want)
	}
	if h.sent != 0 {
		t.Fatalf("Sent was called %d times, want a record to take the turn's path", h.sent)
	}
}

func TestARecordWithoutTextPromptsNothing(t *testing.T) {
	h := &handler{}
	rec, _ := recording(t, commands, h)
	rec.RecordUser(context.Background(), message("  \n\n "))
	rec.RecordAssistant(context.Background(), core.Message{Conversation: "respondio:12345"})

	if len(h.calls) != 0 {
		t.Fatalf("the handler was prompted with %+v, want no prompt", h.calls)
	}
}

func TestAFailedRecordIsLogged(t *testing.T) {
	h := &handler{err: errors.New("the agent exited before it answered")}
	rec, log := recording(t, commands, h)
	rec.RecordUser(context.Background(), message("hello"))

	got := log.String()
	if !strings.Contains(got, "the agent exited before it answered") ||
		!strings.Contains(got, "respondio:12345") || !strings.Contains(got, "/add-user-message") {
		t.Fatalf("log =\n%s\nwant the conversation, the command and the failure", got)
	}
}

// TestWhatTheAgentAnsweredIsLogged pins the only diagnosis an operator has for a
// command the agent does not support: the agent answers with text, not with an
// error, so the text belongs in the log.
func TestWhatTheAgentAnsweredIsLogged(t *testing.T) {
	h := &handler{emit: "/add-user-message is not a command"}
	rec, log := recording(t, commands, h)
	rec.RecordUser(context.Background(), message("hello"))

	if got := log.String(); !strings.Contains(got, "/add-user-message is not a command") {
		t.Fatalf("log =\n%s\nwant what the agent answered", got)
	}
}
