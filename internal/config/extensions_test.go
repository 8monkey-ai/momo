package config

import "testing"

// extension is the block the session history sync decodes for itself.
type extension struct {
	UserMessageCommand      string `yaml:"user_message_command"`
	AssistantMessageCommand string `yaml:"assistant_message_command"`
}

const withExtension = "extensions:\n  session-history-sync:\n" +
	"    user_message_command: /momo-user\n" +
	"    assistant_message_command: /momo-assistant\n"

func TestExtensionBlockDecodesIntoTheExtensionsOwnSettings(t *testing.T) {
	cfg := load(t, withExtension)
	decode, ok := cfg.Extensions["session-history-sync"]
	if !ok {
		t.Fatalf("extensions = %v, want session-history-sync", cfg.Extensions)
	}
	var e extension
	if err := decode(&e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := extension{UserMessageCommand: "/momo-user", AssistantMessageCommand: "/momo-assistant"}
	if e != want {
		t.Fatalf("extension = %+v, want %+v", e, want)
	}
}

// A deployment without human handover configures no extension.
func TestNoExtensionsBlockConfiguresNoExtension(t *testing.T) {
	cfg := load(t, "channels:\n  acp:\n")
	if len(cfg.Extensions) != 0 {
		t.Fatalf("extensions = %v, want none", cfg.Extensions)
	}
}

func TestMisspelledExtensionSettingIsReported(t *testing.T) {
	cfg := load(t, "extensions:\n  session-history-sync:\n    user_mesage_command: /momo-user\n")
	var e extension
	if err := cfg.Extensions["session-history-sync"](&e); err == nil {
		t.Fatal("decode succeeded, want an error naming the unknown setting")
	}
}

func TestEmptyExtensionBlockLeavesDefaults(t *testing.T) {
	cfg := load(t, "extensions:\n  session-history-sync:\n")
	e := extension{UserMessageCommand: "default"}
	if err := cfg.Extensions["session-history-sync"](&e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.UserMessageCommand != "default" {
		t.Fatalf("user_message_command = %q, want the untouched default", e.UserMessageCommand)
	}
}
