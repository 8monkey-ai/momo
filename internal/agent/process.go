package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"syscall"
	"time"

	"github.com/sourcegraph/jsonrpc2"
)

// exitGrace is how long a signalled harness has to store its session and exit
// before momo kills it. It is not a setting: an operator has nothing to gain
// from a harness that takes longer to shut down.
const exitGrace = 5 * time.Second

// start runs the harness in the conversation's directory and opens the JSON-RPC
// connection over its standard input and output.
func (a *Agent) start(ctx context.Context, dir string) (*harness, error) {
	// The operator chooses this command; running it is the point.
	cmd := exec.Command(a.command[0], a.command[1:]...) //nolint:gosec // operator-supplied command
	cmd.Dir = dir
	// The harness's stderr is the only diagnostic a failing harness has.
	cmd.Stderr = &stderrLog{log: a.log}
	// The harness holds the pipes open after it exits only if it left a child
	// behind, and then Wait must not block on the read side for ever.
	cmd.WaitDelay = exitGrace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the harness: %w", err)
	}
	h := &harness{cmd: cmd, log: a.log}
	// The connection's context is the turn's, so a turn that reaches its limit
	// releases every call that waits on the harness.
	h.conn = jsonrpc2.NewConn(ctx, jsonrpc2.NewPlainObjectStream(stdio{stdout, stdin}), h)
	return h, nil
}

// stop leaves no subprocess behind, on every path a turn can take. Closing the
// connection closes the harness's standard input, then the signal asks it to
// exit, and the kill is what follows an exit that does not happen.
func (h *harness) stop() {
	_ = h.conn.Close()
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		if err := h.cmd.Wait(); err != nil {
			h.log.Info("the harness exited with an error", "error", err)
		}
	}()
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		h.log.Info("signalling the harness failed", "error", err)
	}
	select {
	case <-reaped:
		return
	case <-time.After(exitGrace):
	}
	if err := h.cmd.Process.Kill(); err != nil {
		h.log.Warn("killing the harness failed", "error", err)
	}
	<-reaped
}

// stdio joins the subprocess's separate standard input and output into the one
// read-write-closer the JSON-RPC stream reads and writes.
type stdio struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser
}

func (s stdio) Read(p []byte) (int, error)  { return s.stdout.Read(p) }
func (s stdio) Write(p []byte) (int, error) { return s.stdin.Write(p) }

func (s stdio) Close() error {
	err := s.stdin.Close()
	if closed := s.stdout.Close(); err == nil {
		err = closed
	}
	return err
}

// stderrLog writes one log record per line the harness prints, and never message
// content: the harness's stderr is diagnostic output, and the reply travels over
// the protocol.
type stderrLog struct {
	log     *slog.Logger
	pending []byte
}

// stderrLimit is the length after which a line with no end is written as it
// stands: a harness that never ends a line must not grow momo's memory.
const stderrLimit = 64 << 10

func (s *stderrLog) Write(p []byte) (int, error) {
	s.pending = append(s.pending, p...)
	for {
		end := bytes.IndexByte(s.pending, '\n')
		if end < 0 {
			if len(s.pending) < stderrLimit {
				return len(p), nil
			}
			end = len(s.pending)
		}
		s.log.Info("harness stderr", "line", string(bytes.TrimRight(s.pending[:end], "\r")))
		s.pending = s.pending[min(end+1, len(s.pending)):]
	}
}
