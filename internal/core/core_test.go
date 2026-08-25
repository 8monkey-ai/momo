package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
)

// recordingAgent keeps what the last record asked of it, and fails with err.
type recordingAgent struct {
	message Message
	role    Role
	err     error
}

func (a *recordingAgent) Turn(context.Context, Message, Emit) error { return nil }

func (a *recordingAgent) Record(_ context.Context, m Message, role Role) error {
	a.message, a.role = m, role
	return a.err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRecordPassesTheMessageAndTheRoleToTheAgent(t *testing.T) {
	a := &recordingAgent{}
	m := Message{Conversation: "respondio:12345", Content: Text("hello")}

	NewHandler(quiet(), a).Record(context.Background(), m, RoleUser)

	if !reflect.DeepEqual(a.message, m) {
		t.Fatalf("the agent got %+v, want %+v", a.message, m)
	}
	if a.role != "user" {
		t.Fatalf("the agent got the role %q, want %q", a.role, "user")
	}
}

// TestARecordThatFailsStaysInTheLog pins that a channel learns nothing of a
// failed record: it cannot recover from one, so Record answers with nothing.
func TestARecordThatFailsStaysInTheLog(t *testing.T) {
	a := &recordingAgent{err: errors.New("the agent exited before it stored the message")}
	m := Message{Conversation: "respondio:12345", Content: Text("hello")}

	NewHandler(quiet(), a).Record(context.Background(), m, RoleAssistant)

	if a.role != "assistant" {
		t.Fatalf("the agent got the role %q, want %q", a.role, "assistant")
	}
}
