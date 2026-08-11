package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if err := cfg.Channels["respondio"](&s); err != nil {
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
	if err := cfg.Channels["respondio"](&s); err != nil {
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
	if err := cfg.Channels["respondio"](&s); err == nil {
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

func TestMissingFileIsReported(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load succeeded, want an error")
	}
}

func TestAgentBlockDecodesIntoTheAgentsOwnSettings(t *testing.T) {
	cfg := load(t, "agent:\n  command: [\"claude-code-acp\"]\n  data_dir: /var/lib/momo\n")
	var settings struct {
		Command []string `yaml:"command"`
		DataDir string   `yaml:"data_dir"`
	}
	if err := cfg.Agent(&settings); err != nil {
		t.Fatalf("decoding the agent block: %v", err)
	}
	if len(settings.Command) != 1 || settings.Command[0] != "claude-code-acp" {
		t.Errorf("command = %v, want [claude-code-acp]", settings.Command)
	}
	if settings.DataDir != "/var/lib/momo" {
		t.Errorf("data_dir = %q, want \"/var/lib/momo\"", settings.DataDir)
	}
}

func TestMisspelledAgentSettingIsReported(t *testing.T) {
	cfg := load(t, "agent:\n  data_directory: /var/lib/momo\n")
	var settings struct {
		DataDir string `yaml:"data_dir"`
	}
	if err := cfg.Agent(&settings); err == nil {
		t.Fatal("decoding succeeded, want the misspelled setting reported")
	}
}
