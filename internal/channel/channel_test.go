package channel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/core"
	"github.com/8monkey-ai/momo/internal/extension/sessionhistorysync"
)

func noSettings(any) error { return nil }

// configured answers the configuration of every named channel, with no settings
// and no delivery block, the way an operator who names a channel and nothing else
// configures it.
func configured(names ...string) map[string]Config {
	configs := map[string]Config{}
	for _, name := range names {
		configs[name] = Config{Settings: noSettings, Delivery: noSettings}
	}
	return configs
}

// yamlDecoder decodes a block written as YAML, the way config hands one over.
func yamlDecoder(body string) Decoder {
	return func(v any) error {
		dec := yaml.NewDecoder(strings.NewReader(body))
		dec.KnownFields(true)
		if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
}

func stub(name string) Factory {
	return func(context.Context, Decoder, core.Handler, sessionhistorysync.Recorder) (Channel, error) {
		return fixed{routes: []Route{{Path: "/" + name}}}, nil
	}
}

type fixed struct {
	routes []Route
}

func (f fixed) Routes() []Route { return f.routes }

func isolateFactories(t *testing.T) {
	t.Helper()
	saved := factories
	factories = map[string]Factory{}
	t.Cleanup(func() { factories = saved })
}

func TestBuildsRegisteredChannelsInAStableOrder(t *testing.T) {
	isolateFactories(t)
	Register("stub-b", stub("b"))
	Register("stub-a", stub("a"))

	got, err := Build(context.Background(), configured("stub-b", "stub-a"), nil, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) != 2 || got[0].Name != "stub-a" || got[1].Name != "stub-b" {
		t.Fatalf("instances = %+v, want stub-a then stub-b", got)
	}
}

func TestBuildRejectsUnconfiguredChannelName(t *testing.T) {
	if _, err := Build(context.Background(), configured("telegran"), nil, nil); err == nil {
		t.Fatal("Build succeeded, want an error naming the unknown channel")
	}
}

func TestBuildReportsWhichChannelFailed(t *testing.T) {
	isolateFactories(t)
	broken := errors.New("missing signing key")
	Register("stub-broken", func(context.Context, Decoder, core.Handler, sessionhistorysync.Recorder) (Channel, error) {
		return nil, broken
	})

	_, err := Build(context.Background(), configured("stub-broken"), nil, nil)
	if !errors.Is(err, broken) {
		t.Fatalf("error = %v, want it to wrap %v", err, broken)
	}
}

// deliver is a channel that hands one message to the handler it was built with,
// in both directions, so a test can observe what the handler sees. What the
// handler reports about the incoming message reaches failed.
func deliver(conversation string, failed *error) Factory {
	return func(_ context.Context, _ Decoder, h core.Handler, _ sessionhistorysync.Recorder) (Channel, error) {
		m := core.Message{Conversation: conversation, Content: core.Text("hello")}
		err := h.Received(context.Background(), m, func(context.Context, []core.ContentBlock) error { return nil })
		if failed != nil {
			*failed = err
		}
		h.Sent(context.Background(), m)
		return fixed{}, nil
	}
}

type handled struct {
	received []string
	sent     []string
	err      error
}

func (r *handled) Received(_ context.Context, m core.Message, _ core.Reply) error {
	r.received = append(r.received, m.Conversation)
	return r.err
}

func (r *handled) Sent(_ context.Context, m core.Message) {
	r.sent = append(r.sent, m.Conversation)
}

func TestHandlerSeesTheConversationQualifiedWithTheChannelName(t *testing.T) {
	isolateFactories(t)
	Register("respondio", deliver("123", nil))
	Register("acp", deliver("123", nil))
	got := &handled{}

	if _, err := Build(context.Background(), configured("respondio", "acp"), got, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.received) != 2 || got.received[0] != "acp:123" || got.received[1] != "respondio:123" {
		t.Fatalf("received = %v, want [acp:123 respondio:123]", got.received)
	}
}

func TestSentIsQualifiedWithTheChannelName(t *testing.T) {
	isolateFactories(t)
	Register("respondio", deliver("123", nil))
	got := &handled{}

	if _, err := Build(context.Background(), configured("respondio"), got, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.sent) != 1 || got.sent[0] != "respondio:123" {
		t.Fatalf("sent = %v, want [respondio:123]", got.sent)
	}
}

func TestChannelLearnsThatTheTurnFailed(t *testing.T) {
	isolateFactories(t)
	var failed error
	Register("respondio", deliver("123", &failed))
	turn := errors.New("the agent exited before it replied")

	if _, err := Build(context.Background(), configured("respondio"), &handled{err: turn}, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !errors.Is(failed, turn) {
		t.Fatalf("the channel got %v, want it to wrap %v", failed, turn)
	}
}

func TestChannelCannotSupplyTheChannelPartItself(t *testing.T) {
	isolateFactories(t)
	Register("respondio", deliver("acp:123", nil))
	got := &handled{}

	if _, err := Build(context.Background(), configured("respondio"), got, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.received) != 1 || got.received[0] != "respondio:acp:123" {
		t.Fatalf("received = %v, want [respondio:acp:123]", got.received)
	}
}

// replies is a channel that hands one message to the handler and records the text
// of every reply the turn delivered.
func replies(texts *[]string) Factory {
	return func(_ context.Context, _ Decoder, h core.Handler, _ sessionhistorysync.Recorder) (Channel, error) {
		m := core.Message{Conversation: "1", Content: core.Text("hi")}
		record := func(_ context.Context, content []core.ContentBlock) error {
			*texts = append(*texts, core.TextOf(content))
			return nil
		}
		return fixed{}, h.Received(context.Background(), m, record)
	}
}

// twoParagraphs is an agent whose reply holds one blank line.
type twoParagraphs struct{}

func (twoParagraphs) Turn(_ context.Context, _ core.Message, emit core.Emit) error {
	return emit(core.Text("first\n\nsecond"))
}

// TestEachChannelDeliversWithItsOwnSettings pins that delivery belongs to the
// channel: one reply, two channels, two different results in the same run.
func TestEachChannelDeliversWithItsOwnSettings(t *testing.T) {
	isolateFactories(t)
	var split, whole []string
	Register("respondio", replies(&split))
	Register("acp", replies(&whole))
	configs := map[string]Config{
		"respondio": {Settings: noSettings, Delivery: yamlDecoder("separator: \"\\n\\n\"\n")},
		"acp":       {Settings: noSettings, Delivery: noSettings},
	}
	h := core.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), twoParagraphs{})

	if _, err := Build(context.Background(), configs, h, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(split) != 2 || split[0] != "first" || split[1] != "second" {
		t.Fatalf("respondio delivered %q, want two paragraphs", split)
	}
	if len(whole) != 1 || whole[0] != "first\n\nsecond" {
		t.Fatalf("acp delivered %q, want one message", whole)
	}
}

func TestBuildRefusesADeliveryItCannotPace(t *testing.T) {
	isolateFactories(t)
	Register("respondio", stub("respondio"))
	configs := map[string]Config{
		"respondio": {Settings: noSettings, Delivery: yamlDecoder("words_per_minute: -1\n")},
	}

	_, err := Build(context.Background(), configs, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "respondio") || !strings.Contains(err.Error(), "words_per_minute") {
		t.Fatalf("Build error = %v, want it to name the channel and the setting", err)
	}
}

func records(content []core.ContentBlock) Factory {
	return func(_ context.Context, _ Decoder, _ core.Handler, r sessionhistorysync.Recorder) (Channel, error) {
		m := core.Message{Conversation: "123", Content: content}
		return fixed{}, r.Record(context.Background(), m, sessionhistorysync.RoleUser)
	}
}

func passes(got *sessionhistorysync.Recorder) Factory {
	return func(_ context.Context, _ Decoder, _ core.Handler, r sessionhistorysync.Recorder) (Channel, error) {
		*got = r
		return fixed{}, nil
	}
}

type recorded struct {
	conversations []string
	texts         []string
	roles         []sessionhistorysync.Role
}

func (r *recorded) Record(_ context.Context, m core.Message, role sessionhistorysync.Role) error {
	r.conversations = append(r.conversations, m.Conversation)
	r.texts = append(r.texts, core.TextOf(m.Content))
	r.roles = append(r.roles, role)
	return nil
}

func TestTheRecorderSeesTheConversationQualifiedWithTheChannelName(t *testing.T) {
	isolateFactories(t)
	Register("respondio", records(core.Text("hello")))
	got := &recorded{}

	if _, err := Build(context.Background(), configured("respondio"), &handled{}, got); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.conversations) != 1 || got.conversations[0] != "respondio:123" {
		t.Fatalf("recorded = %v, want [respondio:123]", got.conversations)
	}
	if len(got.roles) != 1 || got.roles[0] != sessionhistorysync.RoleUser {
		t.Fatalf("roles = %v, want [user]", got.roles)
	}
}

// TestTheRecorderIsNotPaced pins that delivery wraps the handler only: a record
// answers no contact, so it gets no paragraphs and no pauses.
func TestTheRecorderIsNotPaced(t *testing.T) {
	isolateFactories(t)
	Register("respondio", records(core.Text("first\n\nsecond")))
	configs := map[string]Config{
		"respondio": {Settings: noSettings, Delivery: yamlDecoder("separator: \"\\n\\n\"\nwords_per_minute: 1\n")},
	}
	got := &recorded{}

	if _, err := Build(context.Background(), configs, &handled{}, got); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.texts) != 1 || got.texts[0] != "first\n\nsecond" {
		t.Fatalf("recorded %q, want the message as it was, in one record", got.texts)
	}
}

func TestADisabledExtensionReachesAChannelAsNil(t *testing.T) {
	isolateFactories(t)
	var passed sessionhistorysync.Recorder
	Register("respondio", passes(&passed))

	if _, err := Build(context.Background(), configured("respondio"), &handled{}, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if passed != nil {
		t.Fatalf("the channel got %+v as its recorder, want nil", passed)
	}
}
