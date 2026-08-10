// Package acp serves ACP with momo as the agent: a client connects over the
// streamable HTTP transport, opens a session, and every prompt it sends is a
// message from a contact.
//
// The transport itself is a draft. This implements its RFD, "Streamable HTTP
// and WebSocket transport", as revised 2026-07-02, and serves the HTTP profile
// only.
package acp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/core"
)

func init() {
	channel.Register("acp", New)
}

const (
	connectionHeader = "Acp-Connection-Id"
	sessionHeader    = "Acp-Session-Id"

	// A JSON-RPC message from a client is small, and the whole body is read
	// before it is parsed; the limit keeps that read bounded.
	maxBodyBytes = 1 << 20
)

type settings struct {
	Token           string        `yaml:"token"`
	Path            string        `yaml:"path"`
	ConnectionGrace time.Duration `yaml:"connection_grace"`
}

type acp struct {
	token string
	path  string
	conns *connectionManager
	core  core.Handler
}

func (a *acp) Routes() []channel.Route {
	return []channel.Route{{Path: a.path, Handler: a}}
}

// New configures the ACP channel: one endpoint serving POST, GET and DELETE,
// behind the bearer token the operator set.
func New(decode channel.Decoder, h core.Handler) (channel.Channel, error) {
	s := settings{Path: "/acp/v1", ConnectionGrace: 5 * time.Minute}
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.Token == "" {
		return nil, errors.New("token is required")
	}
	a := &acp{token: s.Token, path: s.Path, conns: newConnectionManager(s.ConnectionGrace), core: h}
	closeStreamsOnSignal(a.conns)
	return a, nil
}

// closeStreamsOnSignal makes momo's shutdown independent of its clients. An SSE
// response never returns on its own, and a channel is handed no shutdown hook,
// so http.Server.Shutdown would wait on every open stream until its timeout.
// The channel watches the same signals momo's own shutdown watches and closes
// its streams itself.
func closeStreamsOnSignal(m *connectionManager) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer signal.Stop(signals)
		select {
		case <-signals:
			m.stop()
		case <-m.done:
		}
	}()
}

func (a *acp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The transport-level identity headers are not credentials, so every request
	// presents the token, and does so before anything else is read or looked up.
	if !a.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
		a.post(w, r)
	case http.MethodGet:
		a.openStream(w, r)
	case http.MethodDelete:
		a.terminate(w, r)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *acp) authorized(r *http.Request) bool {
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(presented), []byte(a.token)) == 1
}

func (a *acp) post(w http.ResponseWriter, r *http.Request) {
	if !jsonBody(r) {
		http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if isBatch(body) {
		http.Error(w, "batched messages are not supported", http.StatusNotImplemented)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, codeParseError, "parse error")
		return
	}
	// Answering under a fabricated id would put the answer on a stream with
	// nothing for the client to correlate it against, so a method that needs an
	// answer is refused without one — before any connection or session is
	// created or looked up.
	if req.notification() && req.Method != methodCancel {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "id is required for "+req.Method)
		return
	}
	if req.Method == methodInitialize {
		a.initialize(w, req)
		return
	}
	a.dispatch(w, r, req)
}

// dispatch answers a request on an established connection. Every such answer
// arrives later, on the stream the client opened for the scope it asked under.
func (a *acp) dispatch(w http.ResponseWriter, r *http.Request, req request) {
	connID, ok := connectionID(w, r)
	if !ok {
		return
	}
	sessionID := r.Header.Get(sessionHeader)
	if sessionScoped[req.Method] && sessionID == "" {
		http.Error(w, "missing "+sessionHeader, http.StatusBadRequest)
		return
	}
	if !a.conns.known(connID, sessionID) {
		http.Error(w, "unknown connection or session", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	result, e := a.call(r.Context(), req, connID, sessionID)
	if result == nil && e == nil {
		return
	}
	a.conns.send(connID, sessionID, answer(req.ID, result, e))
}

func (a *acp) openStream(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "" {
		http.Error(w, "only the HTTP profile of the transport is served", http.StatusNotImplemented)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		http.Error(w, "accept must include text/event-stream", http.StatusNotAcceptable)
		return
	}
	connID, ok := connectionID(w, r)
	if !ok {
		return
	}
	sessionID := r.Header.Get(sessionHeader)
	s, ok := a.conns.listen(connID, sessionID)
	if !ok {
		http.Error(w, "unknown connection or session", http.StatusNotFound)
		return
	}
	defer a.conns.unlisten(connID, sessionID, s)
	writeStream(r.Context(), w, s, a.conns.done)
}

func (a *acp) terminate(w http.ResponseWriter, r *http.Request) {
	connID, ok := connectionID(w, r)
	if !ok {
		return
	}
	if !a.conns.drop(connID) {
		http.Error(w, "unknown connection", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeStream(ctx context.Context, w http.ResponseWriter, s *stream, shutdown <-chan struct{}) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		return
	}
	for {
		select {
		case msg := <-s.messages:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
		case <-s.closed:
			return
		case <-ctx.Done():
			return
		case <-shutdown:
			return
		}
	}
}

func connectionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.Header.Get(connectionHeader)
	if id == "" {
		http.Error(w, "missing "+connectionHeader, http.StatusBadRequest)
		return "", false
	}
	return id, true
}

func jsonBody(r *http.Request) bool {
	t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && t == "application/json"
}
