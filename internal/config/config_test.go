package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestTimeoutsBlockDecodesEveryValue(t *testing.T) {
	cfg := load(t, "timeouts:\n  read_header: 5s\n  read: 15s\n  idle: 1m\n  shutdown: 9s\n")
	want := Timeouts{
		ReadHeader: 5 * time.Second,
		Read:       15 * time.Second,
		Idle:       time.Minute,
		Shutdown:   9 * time.Second,
	}
	if cfg.Timeouts != want {
		t.Errorf("timeouts = %+v, want %+v", cfg.Timeouts, want)
	}
}

func TestAbsentTimeoutsTakeTheirDefaults(t *testing.T) {
	want := Timeouts{
		ReadHeader: defaultReadHeaderTimeout,
		Read:       defaultReadTimeout,
		Idle:       defaultIdleTimeout,
		Shutdown:   defaultShutdownTimeout,
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "no timeouts block at all", body: "channels:\n  respondio:\n"},
		{name: "an empty timeouts block", body: "timeouts:\n"},
		{name: "a zero value per key", body: "timeouts:\n  read_header: 0s\n  read: 0s\n  idle: 0s\n  shutdown: 0s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := load(t, tc.body).Timeouts; got != want {
				t.Errorf("timeouts = %+v, want the defaults %+v", got, want)
			}
		})
	}
}

func TestANegativeTimeoutIsReported(t *testing.T) {
	for _, key := range []string{"read_header", "read", "idle", "shutdown"} {
		t.Run(key, func(t *testing.T) {
			_, err := parse([]byte("timeouts:\n  " + key + ": -1s\n"))
			if err == nil {
				t.Fatalf("parse succeeded, want an error naming %q", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %v, want it to name %q", err, key)
			}
		})
	}
}

func TestMaxConnectionsIsKeptOrTakesItsDefault(t *testing.T) {
	if got := load(t, "max_connections: 4\n").MaxConnections; got != 4 {
		t.Errorf("max_connections = %d, want 4", got)
	}
	if got := load(t, "channels:\n  respondio:\n").MaxConnections; got != defaultMaxConnections {
		t.Errorf("max_connections absent = %d, want the default %d", got, defaultMaxConnections)
	}
	if got := load(t, "max_connections: 0\n").MaxConnections; got != defaultMaxConnections {
		t.Errorf("max_connections = 0 gave %d, want the default %d", got, defaultMaxConnections)
	}
}

func TestANegativeMaxConnectionsIsReported(t *testing.T) {
	_, err := parse([]byte("max_connections: -1\n"))
	if err == nil {
		t.Fatal("parse succeeded, want an error naming max_connections")
	}
	if !strings.Contains(err.Error(), "max_connections") {
		t.Errorf("error = %v, want it to name max_connections", err)
	}
}

func TestMissingFileIsReported(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load succeeded, want an error")
	}
}
