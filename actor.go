package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

type turnRequest struct {
	ctx        context.Context
	blocks     []acp.ContentBlock
	deliver    func(string) error
	recordOnly bool
	errc       chan error
}

// actor runs a contact's turns one at a time on a single goroutine, draining
// its inbox in FIFO order and owning the harness lifecycle — including
// steering: it cancels the in-flight prompt when a newer request arrives.
// inbox and sess are guarded by the manager's mu.
type actor struct {
	m         *manager
	contactID int64
	inbox     []turnRequest
	arrived   chan struct{} // coalesced enqueue signal; buffered 1, sent under mu
	sess      *userSession  // non-nil while a harness is alive
}

// manager maps respond.io contacts to their actors.
type manager struct {
	cfg config

	mu     sync.Mutex
	actors map[int64]*actor
}

func newManager(cfg config) *manager {
	return &manager{
		cfg:    cfg,
		actors: make(map[int64]*actor),
	}
}

// prompt runs one turn, delivering the reply. If a turn is already in flight,
// the actor steers: cancels it, then runs this prompt behind it.
func (m *manager) prompt(ctx context.Context, contactID int64, blocks []acp.ContentBlock, deliver func(string) error) error {
	return m.submit(ctx, contactID, blocks, deliver, false)
}

// record runs a record-only turn: the prompt persists a message into the
// session context and any output is discarded.
func (m *manager) record(ctx context.Context, contactID int64, blocks []acp.ContentBlock) error {
	return m.submit(ctx, contactID, blocks, nil, true)
}

// submit queues the turn on the contact's actor (creating it if needed) and
// waits for the result. Enqueueing under mu makes it atomic with the actor's
// exit check in loop.
func (m *manager) submit(ctx context.Context, contactID int64, blocks []acp.ContentBlock, deliver func(string) error, recordOnly bool) error {
	req := turnRequest{ctx: ctx, blocks: blocks, deliver: deliver, recordOnly: recordOnly, errc: make(chan error, 1)}

	m.mu.Lock()
	a, ok := m.actors[contactID]
	if !ok {
		a = &actor{m: m, contactID: contactID, arrived: make(chan struct{}, 1)}
		m.actors[contactID] = a
		go a.loop()
	}
	a.inbox = append(a.inbox, req)
	select {
	case a.arrived <- struct{}{}:
	default:
	}
	m.mu.Unlock()

	return <-req.errc
}

// loop drains the inbox, one turn at a time. On empty inbox it deregisters
// and exits; check and delete share one lock hold with submit's enqueue, so a
// request either lands before the exit or creates a fresh actor. A harness
// left alive by a cancelled turn is shut down before deregistering, while
// still registered, so a concurrent submit can't spawn a second one.
func (a *actor) loop() {
	for {
		a.m.mu.Lock()
		if len(a.inbox) == 0 {
			if s := a.sess; s != nil {
				a.sess = nil
				a.m.mu.Unlock()
				s.shutdown()
				continue
			}
			delete(a.m.actors, a.contactID)
			a.m.mu.Unlock()
			return
		}
		req := a.inbox[0]
		a.inbox = a.inbox[1:]
		stale := len(a.inbox) > 0
		a.m.mu.Unlock()

		err := a.runTurn(req, stale)
		if errors.Is(err, errHarnessGone) {
			log.Printf("contact %d: harness died mid-prompt, retrying with a fresh one", a.contactID)
			err = a.runTurn(req, stale)
		}
		req.errc <- err
	}
}

var errHarnessGone = errors.New("harness died mid-prompt")

// runTurn prompts the harness with one request and waits for it, steering
// (cancelling the in-flight prompt) when a newer request arrives. A request
// already superseded at pickup (stale) is still prompted so its message
// enters the session, but cancelled as soon as it starts streaming.
func (a *actor) runTurn(req turnRequest, stale bool) error {
	s := a.sess // only the actor goroutine writes a.sess
	if s == nil {
		var err error
		s, err = a.m.spawn(req.ctx, a.contactID)
		if err != nil {
			return err
		}
		a.setSession(s)
	}

	ctx, cancel := context.WithCancel(req.ctx)
	defer cancel()

	steer := func() {
		if err := s.conn.Cancel(ctx, acp.CancelNotification{SessionId: s.sessionID}); err != nil {
			log.Printf("contact %d: cancel: %v", a.contactID, err)
		}
	}

	t := newTurn(req.deliver, a.m.cfg.typingPerWord, req.recordOnly)
	steered := false
	switch {
	case req.recordOnly:
		// The record command's prompt never resolves (pi runs it without an
		// agent loop); end the turn at the ack chunk instead. FUTURE: drop
		// once https://github.com/svkozak/pi-acp/issues/84 is fixed.
		t.onFirstChunk = cancel
	case stale:
		// Cancelling at the first chunk (not before the prompt) guarantees
		// the agent has registered the prompt the cancel targets.
		t.onFirstChunk = steer
		steered = true
	}
	s.turn.Store(t)

	type promptResult struct {
		resp acp.PromptResponse
		err  error
	}
	done := make(chan promptResult, 1)
	go func() {
		resp, err := s.conn.Prompt(ctx, acp.PromptRequest{
			SessionId: s.sessionID,
			Prompt:    req.blocks,
		})
		done <- promptResult{resp, err}
	}()

	var res promptResult
	for done != nil {
		select {
		case res = <-done:
			done = nil
		case <-a.arrived:
			// The coalesced signal may predate this turn; only a non-empty
			// inbox means a newer request supersedes it. Record-only turns
			// self-terminate at the ack chunk and must not be steered.
			a.m.mu.Lock()
			superseded := len(a.inbox) > 0
			a.m.mu.Unlock()
			if superseded && !req.recordOnly && !steered {
				steered = true
				steer()
			}
		}
	}
	resp, err := res.resp, res.err

	s.turn.Store(nil)

	if err != nil {
		t.finish(false)
		// A cancelled ctx on a record-only turn means the ack fired: its
		// expected end, not a failure.
		if req.recordOnly && ctx.Err() != nil {
			a.terminate()
			return nil
		}
		select {
		case <-s.conn.Done():
			a.terminate()
			return errHarnessGone
		default:
		}
		a.terminate()
		return fmt.Errorf("prompt: %w", err)
	}
	t.finish(resp.StopReason != acp.StopReasonCancelled)
	// A cancelled turn means a steering prompt is queued; keep the harness
	// alive so it lands in the live session.
	if resp.StopReason == acp.StopReasonCancelled {
		return nil
	}
	// Terminate after every completed turn: recorded messages and restored
	// history only apply on the next session rebuild (session/load). FUTURE:
	// termination is server-driven only because pi-acp can't survive its pi
	// child dying (https://github.com/svkozak/pi-acp/issues/82); once fixed,
	// the harness could end its own session instead.
	a.terminate()
	return nil
}

// setSession publishes a.sess under mu for stopAll; the actor goroutine, the
// sole writer, reads it without the lock.
func (a *actor) setSession(s *userSession) {
	a.m.mu.Lock()
	a.sess = s
	a.m.mu.Unlock()
}

func (a *actor) terminate() {
	s := a.sess
	a.setSession(nil)
	s.shutdown()
}

// stopAll shuts down every live harness on server shutdown.
func (m *manager) stopAll() {
	m.mu.Lock()
	sessions := make([]*userSession, 0, len(m.actors))
	for _, a := range m.actors {
		if a.sess != nil {
			sessions = append(sessions, a.sess)
		}
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Go(s.shutdown)
	}
	wg.Wait()
}
