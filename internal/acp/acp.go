// Package acp serves momo's ACP endpoint: momo is the agent side of the Agent
// Client Protocol, so a prompt an ACP client sends is a message from a contact.
//
// It speaks protocol version 1 over the streamable HTTP transport described in
// the RFD "Streamable HTTP & WebSocket Transport", revision 2026-07-02. That
// transport is a draft in both published protocol versions, and the WebSocket
// profile the RFD also defines is not served here.
package acp

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/core"
)

func init() {
	channel.Register("acp", New)
}

const (
	maxBodyBytes = 1 << 20

	connectionHeader = "Acp-Connection-Id"
	sessionHeader    = "Acp-Session-Id"

	eventStream = "text/event-stream"
	jsonType    = "application/json"
)

type settings struct {
	Token string `yaml:"token"`
	Path  string `yaml:"path"`
}

type endpoint struct {
	token    string
	routes   []channel.Route
	hub      *hub
	core     core.Handler
	shutdown chan struct{}
	once     sync.Once
}

// New configures the ACP channel: one endpoint serving POST, GET and DELETE,
// with a bearer token the operator sets.
func New(decode channel.Decoder, h core.Handler) (channel.Channel, error) {
	s := settings{Path: "/acp"}
	if err := decode(&s); err != nil {
		return nil, err
	}
	if s.Token == "" {
		return nil, errors.New("token is required")
	}
	e := &endpoint{token: s.Token, hub: newHub(), core: h, shutdown: make(chan struct{})}
	e.routes = []channel.Route{{Path: s.Path, Handler: e}}
	return e, nil
}

func (e *endpoint) Routes() []channel.Route { return e.routes }

// momo releases a channel's long-lived responses by asking it for this shape
// when it shuts down, so the assertion keeps the signature matching.
var _ interface{ Close() } = (*endpoint)(nil)

// Close ends every open stream, so the SSE responses momo is holding do not
// keep a shutdown waiting for them.
func (e *endpoint) Close() {
	e.once.Do(func() { close(e.shutdown) })
}

func (e *endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The token is checked before the body is read and before any identifier is
	// looked up: Acp-Connection-Id and Acp-Session-Id are not credentials.
	if !e.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
		e.post(w, r)
	case http.MethodGet:
		e.open(w, r)
	case http.MethodDelete:
		e.terminate(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (e *endpoint) authorized(r *http.Request) bool {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(e.token)) == 1
}

// post carries one client-to-server message. Only initialize is answered in this
// response; every other message is acknowledged with 202 and answered on a
// stream, correlated by JSON-RPC id.
func (e *endpoint) post(w http.ResponseWriter, r *http.Request) {
	m, ok := decodeMessage(w, r)
	if !ok {
		return
	}
	if m.Method == methodInitialize {
		e.initialize(w, m)
		return
	}
	c, sessionID, status := e.identify(r, m.Method)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	res, rpcErr := e.dispatch(r.Context(), c, sessionID, m)
	// A notification has no id and gets nothing back.
	if len(m.ID) > 0 {
		// Exactly one of res and rpcErr is set, and both fields are omitted when empty.
		b, _ := json.Marshal(response{JSONRPC: jsonrpcVersion, ID: m.ID, Result: res, Error: rpcErr})
		c.send(sessionID, b)
	}
	w.WriteHeader(http.StatusAccepted)
}

// decodeMessage validates the request and reads the JSON-RPC message out of it,
// answering the client itself when either fails.
func decodeMessage(w http.ResponseWriter, r *http.Request) (message, bool) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != jsonType {
		http.Error(w, "content type must be "+jsonType, http.StatusUnsupportedMediaType)
		return message{}, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return message{}, false
	}
	if isBatch(body) {
		http.Error(w, "batched messages are not supported", http.StatusNotImplemented)
		return message{}, false
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		writeJSON(w, http.StatusBadRequest, failure(nil, codeParseError, "malformed JSON-RPC message"))
		return message{}, false
	}
	if m.JSONRPC != jsonrpcVersion || m.Method == "" {
		writeJSON(w, http.StatusBadRequest, failure(m.ID, codeInvalidRequest, "not a JSON-RPC "+jsonrpcVersion+" message"))
		return message{}, false
	}
	return m, true
}

// initialize issues the connection id, in the response body and in the header
// every later request must echo.
func (e *endpoint) initialize(w http.ResponseWriter, m message) {
	id, ok := e.hub.open()
	if !ok {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set(connectionHeader, id)
	writeJSON(w, http.StatusOK, response{JSONRPC: jsonrpcVersion, ID: m.ID, Result: initializeResult{
		ProtocolVersion: protocolVersion,
		ConnectionID:    id,
		AuthMethods:     []struct{}{},
	}})
}

// open serves the stream momo writes server-to-client messages on: session-scoped
// when the request carries a session id, connection-scoped otherwise.
func (e *endpoint) open(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "" {
		http.Error(w, "only the streamable HTTP profile is served", http.StatusNotImplemented)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), eventStream) {
		http.Error(w, "Accept must include "+eventStream, http.StatusNotAcceptable)
		return
	}
	c, sessionID, status := e.scope(r)
	if status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	flusher, streamable := w.(http.Flusher)
	if !streamable {
		http.Error(w, "streaming is unavailable", http.StatusInternalServerError)
		return
	}
	s := newStream()
	c.attach(sessionID, s)
	w.Header().Set("Content-Type", eventStream)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	s.writeTo(w, flusher, r.Context().Done(), e.shutdown)
}

// terminate ends the connection, which closes its streams and releases its
// sessions.
func (e *endpoint) terminate(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(connectionHeader)
	if id == "" {
		http.Error(w, connectionHeader+" is required", http.StatusBadRequest)
		return
	}
	if !e.hub.remove(id) {
		http.Error(w, "unknown connection", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// identify resolves the identity headers of a message other than initialize,
// returning the connection, the session scope its response belongs to, and the
// status to refuse the request with, or zero when it is in order.
func (e *endpoint) identify(r *http.Request, method string) (*connection, string, int) {
	c, status := e.connection(r)
	if status != 0 {
		return nil, "", status
	}
	if !sessionScoped(method) {
		return c, "", 0
	}
	sessionID := r.Header.Get(sessionHeader)
	if sessionID == "" {
		return nil, "", http.StatusBadRequest
	}
	if !c.hasSession(sessionID) {
		return nil, "", http.StatusNotFound
	}
	return c, sessionID, 0
}

// scope resolves the identity headers of a stream request, where the session
// header is optional and chooses the scope.
func (e *endpoint) scope(r *http.Request) (*connection, string, int) {
	c, status := e.connection(r)
	if status != 0 {
		return nil, "", status
	}
	sessionID := r.Header.Get(sessionHeader)
	if sessionID != "" && !c.hasSession(sessionID) {
		return nil, "", http.StatusNotFound
	}
	return c, sessionID, 0
}

func (e *endpoint) connection(r *http.Request) (*connection, int) {
	id := r.Header.Get(connectionHeader)
	if id == "" {
		return nil, http.StatusBadRequest
	}
	c, known := e.hub.lookup(id)
	if !known {
		return nil, http.StatusNotFound
	}
	return c, 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonType)
	w.WriteHeader(status)
	// Nothing can be done if the client hung up mid-response.
	_ = json.NewEncoder(w).Encode(v)
}
