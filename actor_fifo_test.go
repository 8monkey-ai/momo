package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// Pins FIFO ordering: prompts queued behind an in-flight turn reach the agent
// in enqueue order. Order is observed at the agent — the testagent appends
// every prompt it receives to .testagent-prompts.log — not via replies, since
// the actor steers superseded turns (cancels them at first chunk). The first
// turn blocks inside the agent ("block:" prefix) until a release file appears,
// and each follower's enqueue is confirmed via the actor's inbox length before
// the next is submitted, so enqueue order is deterministic.
func TestActorProcessesPromptsInFIFOOrder(t *testing.T) {
	cfg := config{agentCmd: testAgentBin(), dataDir: t.TempDir()}
	m := newManager(cfg)

	const contactID = 99
	contactDir := filepath.Join(cfg.dataDir, "99")
	logPath := filepath.Join(contactDir, ".testagent-prompts.log")

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", desc)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	prompts := []string{"block:hold", "msg-1", "msg-2", "msg-3"}
	errs := make(chan error, len(prompts))
	submit := func(text string) {
		go func() {
			errs <- m.prompt(context.Background(), contactID, []acp.ContentBlock{acp.TextBlock(text)}, func(string) error { return nil })
		}()
	}

	// First turn: submitted and confirmed in flight (the agent logged it and
	// is now blocked on the release file).
	submit(prompts[0])
	waitFor("first prompt to reach the agent", func() bool {
		b, err := os.ReadFile(logPath)
		return err == nil && strings.Contains(string(b), prompts[0])
	})

	// Followers: one at a time, each confirmed enqueued before the next. The
	// actor can't dequeue them while the first turn blocks, so the inbox
	// length pins each enqueue.
	for i, text := range prompts[1:] {
		submit(text)
		waitInboxLen(t, m, contactID, i+1)
	}

	if err := os.WriteFile(filepath.Join(contactDir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for range prompts {
		if err := <-errs; err != nil {
			t.Fatalf("prompt: %v", err)
		}
	}

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(b))
	if strings.Join(got, " ") != strings.Join(prompts, " ") {
		t.Fatalf("prompts reached the agent as %q, want %q", got, prompts)
	}
}
