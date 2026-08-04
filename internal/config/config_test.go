package config

import (
	"os"
	"path/filepath"
	"testing"
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
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q, want the default %q", cfg.Listen, DefaultListen)
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
	if cfg := load(t, ""); cfg.Listen != DefaultListen || len(cfg.Channels) != 0 {
		t.Errorf("config = %+v, want defaults and no channels", cfg)
	}
}

func TestRejectsUnknownAndMalformedSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "misspelled setting", body: "listten: \":9000\"\n"},
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
