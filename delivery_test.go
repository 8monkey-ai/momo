package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestChunkingOnParagraphs(t *testing.T) {
	var mu sync.Mutex
	var got []string
	tn := newTurn(func(s string) error {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
		return nil
	}, 0, false)
	tn.addChunk("first para")
	tn.addChunk("graph\n\nsecond")
	tn.addChunk(" paragraph\n\n")
	tn.addChunk("third")
	tn.finish(true)

	want := []string{"first paragraph", "second paragraph", "third"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestChunkingCancelledDropsTail(t *testing.T) {
	var got []string
	tn := newTurn(func(s string) error { got = append(got, s); return nil }, 0, false)
	tn.addChunk("done\n\npartial tail")
	tn.finish(false)
	if len(got) != 1 || got[0] != "done" {
		t.Fatalf("got %q, want [done]", got)
	}
}

func TestTypingDelayPerWord(t *testing.T) {
	if d := typingDelay("three word reply", time.Second); d != 3*time.Second {
		t.Fatalf("got %v, want 3s", d)
	}
}
