package acp

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/8monkey-ai/momo/internal/core"
)

const (
	connectionHeader = "Acp-Connection-Id"
	sessionHeader    = "Acp-Session-Id"

	// A JSON-RPC message momo answers is small; the limit keeps a body momo has
	// to buffer before parsing bounded.
	maxBodyBytes = 1 << 20
)

type endpoint struct {
	token string
	conns *connectionManager
	core  core.Handler
}

func (e *endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Transport-level identifiers are not auth tokens, so every method presents
	// the token and is refused before its body is read or an identity looked up.
	if !e.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
		e.post(w, r)
	case http.MethodGet:
		e.get(w, r)
	case http.MethodDelete:
		e.terminate(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (e *endpoint) authorized(r *http.Request) bool {
	// The scheme token is case-insensitive per RFC 7235, and clients do send
	// "bearer".
	scheme, presented, found := strings.Cut(r.Header.Get("Authorization"), " ")
	return found && strings.EqualFold(scheme, "Bearer") &&
		subtle.ConstantTimeCompare([]byte(presented), []byte(e.token)) == 1
}

func (e *endpoint) post(w http.ResponseWriter, r *http.Request) {
	if mediaType(r) != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, jsonrpc2.CodeParseError, "unreadable body")
		return
	}
	if isBatch(body) {
		writeError(w, http.StatusNotImplemented, jsonrpc2.CodeInvalidRequest, "batch requests are not supported")
		return
	}
	var req jsonrpc2.Request
	if err := json.Unmarshal(body, &req); err != nil {
		// A body that is valid JSON but not a JSON-RPC message is an invalid
		// request, not a parse error.
		code := int64(jsonrpc2.CodeParseError)
		if json.Valid(body) {
			code = jsonrpc2.CodeInvalidRequest
		}
		writeError(w, http.StatusBadRequest, code, "malformed JSON-RPC message")
		return
	}
	// A notification has no id to answer under, and session/cancel is v1's only
	// client-to-server notification. Refusing here keeps momo from answering
	// under a fabricated id on a stream where nothing correlates with it.
	if req.Notif && req.Method != methodCancel {
		writeError(w, http.StatusBadRequest, jsonrpc2.CodeInvalidRequest, fmt.Sprintf("%s requires an id", req.Method))
		return
	}
	e.dispatch(w, r, &req)
}

func (e *endpoint) dispatch(w http.ResponseWriter, r *http.Request, req *jsonrpc2.Request) {
	// initialize is the one method answered in the response to the POST that
	// carried it, because it is what issues the connection id.
	if req.Method == methodInitialize {
		e.initialize(w, req)
		return
	}
	connID := r.Header.Get(connectionHeader)
	if connID == "" {
		http.Error(w, connectionHeader+" is required", http.StatusBadRequest)
		return
	}
	sessionID := r.Header.Get(sessionHeader)
	if sessionScoped(req.Method) && sessionID == "" {
		http.Error(w, sessionHeader+" is required for "+req.Method, http.StatusBadRequest)
		return
	}
	if !e.conns.exists(connID, sessionID) {
		http.Error(w, "unknown connection or session", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	resp := e.answer(r.Context(), req, connID, sessionID)
	if resp == nil {
		return
	}
	// A response the client is no longer listening for is nothing momo can act on:
	// the POST it belongs to has already been answered 202.
	_ = e.conns.send(connID, streamOf(req.Method, sessionID), frame(resp))
}

func (e *endpoint) get(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "" {
		http.Error(w, "the WebSocket profile is not served", http.StatusNotImplemented)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		http.Error(w, "Accept must include text/event-stream", http.StatusNotAcceptable)
		return
	}
	connID := r.Header.Get(connectionHeader)
	if connID == "" {
		http.Error(w, connectionHeader+" is required", http.StatusBadRequest)
		return
	}
	sessionID := r.Header.Get(sessionHeader)
	s, known := e.conns.listen(connID, sessionID)
	if !known {
		http.Error(w, "unknown connection or session", http.StatusNotFound)
		return
	}
	defer e.conns.stopListening(connID, sessionID, s)
	e.stream(w, r, s)
}

func (e *endpoint) stream(w http.ResponseWriter, r *http.Request, s *stream) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// Nothing can be done if the client hung up mid-stream.
	_ = rc.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closed:
			return
		case f := <-s.frames:
			if _, err := w.Write(f); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

func (e *endpoint) terminate(w http.ResponseWriter, r *http.Request) {
	connID := r.Header.Get(connectionHeader)
	if connID == "" {
		http.Error(w, connectionHeader+" is required", http.StatusBadRequest)
		return
	}
	if !e.conns.release(connID) {
		http.Error(w, "unknown connection", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func mediaType(r *http.Request) string {
	t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return t
}

func isBatch(body []byte) bool {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	return strings.HasPrefix(trimmed, "[")
}

// frame is the one place SSE framing is decided. Both a response and a
// notification reach it already encodable: result and SetParams reject a payload
// that does not marshal before it gets here.
func frame(msg json.Marshaler) []byte {
	body, _ := msg.MarshalJSON()
	return append(append([]byte("data: "), body...), '\n', '\n')
}

// writeError answers a message momo refused before it could know the id to
// answer under. jsonrpc2.Response cannot carry the null id JSON-RPC 2.0 requires
// in that case, so only the envelope is built here.
func writeError(w http.ResponseWriter, status int, code int64, message string) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error":   &jsonrpc2.Error{Code: code, Message: message},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
