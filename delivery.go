package main

import (
	"log"
	"strings"
	"sync"
	"time"
)

// turn accumulates streamed agent text and delivers it as separate messages
// split on paragraph boundaries (\n\n), pacing each send like human typing.
// A nil deliver func discards the output (record-only turns).
type turn struct {
	deliver func(string) error
	perWord time.Duration

	// onFirstChunk fires once; record-only turns treat it as the command's ack.
	onFirstChunk func()

	mu     sync.Mutex
	buf    strings.Builder
	closed bool
	queue  chan string
	done   chan struct{}
}

func newTurn(deliver func(string) error, perWord time.Duration) *turn {
	t := &turn{
		deliver: deliver,
		perWord: perWord,
		queue:   make(chan string, 64),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(t.done)
		for p := range t.queue {
			if t.deliver == nil {
				continue
			}
			time.Sleep(typingDelay(p, t.perWord))
			if err := t.deliver(p); err != nil {
				log.Printf("deliver: %v", err)
			}
		}
	}()
	return t
}

func (t *turn) addChunk(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.onFirstChunk != nil {
		t.onFirstChunk()
		t.onFirstChunk = nil
	}
	if t.closed {
		return
	}
	t.buf.WriteString(text)
	parts := strings.Split(t.buf.String(), "\n\n")
	t.buf.Reset()
	t.buf.WriteString(parts[len(parts)-1])
	for _, p := range parts[:len(parts)-1] {
		if p = strings.TrimSpace(p); p != "" {
			t.queue <- p
		}
	}
}

// finish flushes the trailing paragraph (unless cancelled) and waits for all
// queued deliveries to complete.
func (t *turn) finish(flush bool) {
	t.mu.Lock()
	if rest := strings.TrimSpace(t.buf.String()); flush && rest != "" {
		t.queue <- rest
	}
	t.closed = true
	t.mu.Unlock()
	close(t.queue)
	<-t.done
}

func typingDelay(s string, perWord time.Duration) time.Duration {
	return time.Duration(len(strings.Fields(s))) * perWord
}
