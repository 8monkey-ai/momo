package agent

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/core"
	"github.com/8monkey-ai/momo/internal/sessionhistory"
)

// held is a prompt the stub agent holds open until the test releases it.
type held struct {
	listener net.Listener
}

func holding(t *testing.T) held {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	t.Setenv("STUBAGENT_SYNC_ADDR", l.Addr().String())
	return held{listener: l}
}

// arrived waits for one prompt to reach the stub agent. It answers nil when none
// arrived within the wait, so a test states both what must arrive and what must
// not.
func (h held) arrived(t *testing.T, within time.Duration) net.Conn {
	t.Helper()
	if err := h.listener.(*net.TCPListener).SetDeadline(time.Now().Add(within)); err != nil {
		t.Fatal(err)
	}
	conn, err := h.listener.Accept()
	if err != nil {
		return nil
	}
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		t.Fatalf("reading the prompt's mark: %v", err)
	}
	return conn
}

func release(t *testing.T, conn net.Conn) {
	t.Helper()
	if _, err := conn.Write([]byte{'.'}); err != nil {
		t.Fatalf("releasing a prompt: %v", err)
	}
	_ = conn.Close()
}

func history(t *testing.T, h *Harness) core.History {
	t.Helper()
	s, err := sessionhistory.New(discard(), decoder(
		"user_message_command: /momo-user\nassistant_message_command: /momo-assistant\n"), h)
	if err != nil {
		t.Fatalf("sessionhistory.New: %v", err)
	}
	return s
}

// The session never holds two prompts of one conversation at once, so the history
// keeps the order the contact saw.
func TestARecordAndATurnOfOneConversationDoNotRunAtTheSameTime(t *testing.T) {
	prompts := holding(t)
	h, _ := harness(t)
	records := history(t, h)

	done := make(chan struct{}, 2)
	go func() {
		records.RecordUser(context.Background(), core.Message{
			Conversation: "respondio:1", Content: core.Text("the operator answers this one"),
		})
		done <- struct{}{}
	}()
	go func() {
		m := core.Message{Conversation: "respondio:1", Content: core.Text("hi")}
		if err := h.Turn(context.Background(), m, func([]core.ContentBlock) error { return nil }); err != nil {
			t.Errorf("Turn: %v", err)
		}
		done <- struct{}{}
	}()

	first := prompts.arrived(t, 5*time.Second)
	if first == nil {
		t.Fatal("no prompt reached the agent")
	}
	if second := prompts.arrived(t, 300*time.Millisecond); second != nil {
		t.Fatal("a second prompt of the same conversation ran while the first was still open")
	}
	release(t, first)
	second := prompts.arrived(t, 5*time.Second)
	if second == nil {
		t.Fatal("the second prompt of the conversation never ran")
	}
	release(t, second)
	for range 2 {
		<-done
	}
}

// No prompt is released before both arrived, so an implementation that serialises
// every conversation behind one lock fails by deadlock and not by timing.
func TestARecordDoesNotHoldUpAnotherConversation(t *testing.T) {
	prompts := holding(t)
	h, _ := harness(t)
	records := history(t, h)

	done := make(chan struct{}, 2)
	go func() {
		records.RecordUser(context.Background(), core.Message{
			Conversation: "respondio:1", Content: core.Text("the operator answers this one"),
		})
		done <- struct{}{}
	}()
	go func() {
		m := core.Message{Conversation: "respondio:2", Content: core.Text("hi")}
		if err := h.Turn(context.Background(), m, func([]core.ContentBlock) error { return nil }); err != nil {
			t.Errorf("Turn: %v", err)
		}
		done <- struct{}{}
	}()

	open := make([]net.Conn, 0, 2)
	for range 2 {
		conn := prompts.arrived(t, 5*time.Second)
		if conn == nil {
			t.Fatal("a prompt of another conversation waited for the record")
		}
		open = append(open, conn)
	}
	for _, conn := range open {
		release(t, conn)
	}
	for range 2 {
		<-done
	}
}
