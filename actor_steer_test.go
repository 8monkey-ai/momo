package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// recorder collects one prompt's delivered messages.
type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) deliver(s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, s)
	return nil
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.msgs...)
}

func waitInboxLen(t *testing.T, m *manager, contactID int64, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		a := m.actors[contactID]
		l := -1
		if a != nil {
			l = len(a.inbox)
		}
		m.mu.Unlock()
		if l == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("inbox never reached length %d", n)
}

// A prompt queued behind an active turn but superseded by a newer one is still
// prompted — so its message enters the session context — but steered: cancelled
// at its first streamed chunk, its reply never delivered. Only the newest
// request gets a full, delivered turn.
func TestSteerSkipsDeliveryOfSupersededPrompt(t *testing.T) {
	const contactID = 99
	cfg := config{agentCmd: testAgentBin(), dataDir: t.TempDir()}
	m := newManager(cfg)
	ctx := context.Background()

	text := func(s string) []acp.ContentBlock { return []acp.ContentBlock{acp.TextBlock(s)} }
	prompt := func(s string, r *recorder) chan error {
		errc := make(chan error, 1)
		go func() { errc <- m.prompt(ctx, contactID, text(s), r.deliver) }()
		return errc
	}

	// Turn 0: the testagent streams one paragraph, then blocks in-flight
	// (surviving session/cancel) until the release file appears.
	release := filepath.Join(t.TempDir(), "release")
	rec0 := &recorder{}
	errc0 := prompt("hold:"+release, rec0)
	deadline := time.Now().Add(5 * time.Second)
	for len(rec0.got()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("turn 0 never started streaming")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Enqueue A, then B, strictly in order, while turn 0 is still in flight.
	recA, recB := &recorder{}, &recorder{}
	errcA := prompt("cancelme:A", recA)
	waitInboxLen(t, m, contactID, 1)
	errcB := prompt("B-final", recB)
	waitInboxLen(t, m, contactID, 2)

	// Let turn 0 resolve; A is now picked up already superseded by B.
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for name, errc := range map[string]chan error{"turn 0": errc0, "A": errcA, "B": errcB} {
		if err := <-errc; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// A reached the agent: its message is in the session context, after the
	// hold prompt and before B.
	log, err := os.ReadFile(filepath.Join(cfg.dataDir, "99", ".testagent-prompts.log"))
	if err != nil {
		t.Fatalf("reading prompt log: %v", err)
	}
	prompts := strings.Split(strings.TrimSpace(string(log)), "\n")
	want := []string{"hold:" + release, "cancelme:A", "B-final"}
	if !slices.Equal(prompts, want) {
		t.Fatalf("agent saw prompts %q, want %q", prompts, want)
	}

	// A was cancelled at its first chunk: its reply is never delivered.
	if msgs := recA.got(); len(msgs) != 0 {
		t.Errorf("superseded prompt A delivered %q, want nothing", msgs)
	}

	// B ran an uncancelled turn and its reply arrived in full.
	wantB := []string{"You said: B-final", "Second paragraph."}
	if msgs := recB.got(); !slices.Equal(msgs, wantB) {
		t.Errorf("B delivered %q, want %q", msgs, wantB)
	}
}
