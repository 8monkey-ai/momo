package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
)

func load(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "momo.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestMinimalConfigTakesDefaults(t *testing.T) {
	cfg := load(t, "channels:\n  respondio:\n    received_secret: a\n")
	if cfg.Listen != ":8080" {
		t.Errorf("listen = %q, want the default \":8080\"", cfg.Listen)
	}
	if cfg.MaxConnections != 1024 {
		t.Errorf("max_connections = %d, want the default 1024", cfg.MaxConnections)
	}
	if cfg.ReadHeaderTimeout != 10*time.Second || cfg.ReadTimeout != 30*time.Second ||
		cfg.IdleTimeout != 2*time.Minute || cfg.ShutdownTimeout != 20*time.Second {
		t.Errorf("timeouts = %+v, want 10s, 30s, 2m and 20s", cfg)
	}
	if _, ok := cfg.Channels["respondio"]; !ok {
		t.Fatalf("channels = %v, want respondio", cfg.Channels)
	}
}

func TestChannelBlockDecodesIntoTheChannelsOwnSettings(t *testing.T) {
	cfg := load(t, "listen: \":9000\"\nchannels:\n  respondio:\n    received_secret: a\n")
	if cfg.Listen != ":9000" {
		t.Errorf("listen = %q, want \":9000\"", cfg.Listen)
	}
	var s struct {
		ReceivedSecret string `yaml:"received_secret"`
	}
	if err := cfg.Channels["respondio"].Settings(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.ReceivedSecret != "a" {
		t.Errorf("received_secret = %q, want \"a\"", s.ReceivedSecret)
	}
}

func TestEmptyChannelBlockLeavesDefaults(t *testing.T) {
	cfg := load(t, "channels:\n  respondio:\n")
	s := struct {
		Path string `yaml:"path"`
	}{Path: "default"}
	if err := cfg.Channels["respondio"].Settings(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Path != "default" {
		t.Errorf("path = %q, want the untouched default", s.Path)
	}
}

func TestMisspelledChannelSettingIsReported(t *testing.T) {
	cfg := load(t, "channels:\n  respondio:\n    recieved_secret: a\n")
	var s struct {
		ReceivedSecret string `yaml:"received_secret"`
	}
	if err := cfg.Channels["respondio"].Settings(&s); err == nil {
		t.Fatal("decode succeeded, want an error naming the unknown setting")
	}
}

func TestEmptyFileIsValid(t *testing.T) {
	if cfg := load(t, ""); cfg.Listen != ":8080" || len(cfg.Channels) != 0 {
		t.Errorf("config = %+v, want defaults and no channels", cfg)
	}
}

func TestRejectsUnknownAndMalformedSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "misspelled setting", body: "listten: \":9000\"\n"},
		// It would reach the listener as a channel of negative capacity.
		{name: "negative max_connections", body: "max_connections: -1\n"},
		{name: "not yaml", body: "listen: \":9000\"\n  channels:\n"},
		// Only the first document is used, so a second one would be dropped in silence.
		{name: "second document", body: "listen: \":8080\"\n---\nlisten: \":9999\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parse([]byte(tc.body)); err == nil {
				t.Fatal("parse succeeded, want an error the operator can act on")
			}
		})
	}
}

// historyBlock is what the session history sync extension decodes for itself.
type historyBlock struct {
	UserCommand      string `yaml:"user_command"`
	AssistantCommand string `yaml:"assistant_command"`
}

func TestTheSessionHistoryBlockIsDecodedForTheExtension(t *testing.T) {
	cfg := load(t, "session_history:\n  user_command: \"/history-user\"\n  assistant_command: \"/history-assistant\"\n")
	var got historyBlock
	if err := cfg.SessionHistory(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UserCommand != "/history-user" || got.AssistantCommand != "/history-assistant" {
		t.Fatalf("session_history = %+v, want /history-user and /history-assistant", got)
	}
}

func TestNoSessionHistoryBlockLeavesTheExtensionUntouched(t *testing.T) {
	cfg := load(t, "listen: \":9000\"\n")
	got := historyBlock{UserCommand: "untouched"}
	if err := cfg.SessionHistory(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != (historyBlock{UserCommand: "untouched"}) {
		t.Fatalf("session_history = %+v, want the untouched value", got)
	}
}

func TestMisspelledSessionHistorySettingIsReported(t *testing.T) {
	cfg := load(t, "session_history:\n  user_commnad: \"/history-user\"\n")
	var got historyBlock
	if err := cfg.SessionHistory(&got); err == nil {
		t.Fatal("decode succeeded, want an error naming the unknown setting")
	}
}

func TestMissingFileIsReported(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load succeeded, want an error")
	}
}

// delivery is the delivery a channel of the loaded configuration is built with.
func delivery(t *testing.T, cfg *Config, name string) core.Delivery {
	t.Helper()
	d, err := core.NewDelivery(cfg.Channels[name].Delivery)
	if err != nil {
		t.Fatalf("delivery of %q: %v", name, err)
	}
	return d
}

func TestNoDeliveryBlockSendsTheReplyAsOneMessage(t *testing.T) {
	cfg := load(t, "channels:\n  respondio:\n    api_token: a\n")
	if got := delivery(t, cfg, "respondio"); got.Separator != "" || got.WordsPerMinute != 0 || got.MaxDelay != 10*time.Minute {
		t.Fatalf("delivery = %+v, want no separator, no pace and a 10m cap", got)
	}
}

func TestTheDeliveryBlockIsDecodedAndKeptFromTheChannel(t *testing.T) {
	cfg := load(t, "channels:\n  respondio:\n    api_token: a\n    delivery:\n"+
		"      separator: \"---\"\n      words_per_minute: 60\n      max_delay: \"30s\"\n")
	got := delivery(t, cfg, "respondio")
	if got.Separator != "---" || got.WordsPerMinute != 60 || got.MaxDelay != 30*time.Second {
		t.Fatalf("delivery = %+v, want ---, 60 and 30s", got)
	}
	// The channel decodes its own block strictly, so a delivery key still in it
	// would be an unknown setting here.
	var settings struct {
		APIToken string `yaml:"api_token"`
	}
	if err := cfg.Channels["respondio"].Settings(&settings); err != nil {
		t.Fatalf("the channel saw the delivery block: %v", err)
	}
	if settings.APIToken != "a" {
		t.Fatalf("api_token = %q, want \"a\"", settings.APIToken)
	}
}

func TestADeliveryMomoCannotPaceIsRefused(t *testing.T) {
	for name, block := range map[string]string{
		"negative words_per_minute": "      words_per_minute: -1\n",
		"max_delay of zero":         "      max_delay: 0s\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := load(t, "channels:\n  respondio:\n    delivery:\n"+block)
			if _, err := core.NewDelivery(cfg.Channels["respondio"].Delivery); err == nil {
				t.Fatal("the delivery was accepted, want an error naming the setting")
			}
		})
	}
}
