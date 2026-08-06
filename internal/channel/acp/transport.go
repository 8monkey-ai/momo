package acp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
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

	jsonMediaType = "application/json"
	sseMediaType  = "text/event-stream"

	// A prompt now carries whatever content blocks the client sends, so a
	// base64 image or audio block makes this a real ceiling rather than a
	// formality; it keeps a client from making momo buffer without bound.
	maxBodyBytes = 1 << 20
)

// handler serves the single ACP endpoint. Transport failures are answered with
// an HTTP status; a failure of a message momo accepted is answered with a
// JSON-RPC error on the stream that message's response belongs to.
type handler struct {
	token string
	core  core.Handler
	conns *connections
	// life is the channel's lifetime: it is done once momo starts shutting down,
	// which ends the open streams and refuses new connections.
	life context.Context
}

// reject is an HTTP-level refusal: the request never becomes an ACP message.
type reject struct {
	status int
	reason string
}

func (rj reject) write(w http.ResponseWriter) { http.Error(w, rj.reason, rj.status) }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The token is checked first, so an unauthenticated request is refused before
	// its body is read and before any connection or session is looked up.
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
		h.post(w, r)
	case http.MethodGet:
		h.openStream(w, r)
	case http.MethodDelete:
		h.terminate(w, r)
	default:
		w.Header().Set("Allow", "POST, GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handler) post(w http.ResponseWriter, r *http.Request) {
	if baseMediaType(r.Header.Get("Content-Type")) != jsonMediaType {
		http.Error(w, "expected Content-Type: "+jsonMediaType, http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	req, rj := parse(body)
	if rj != nil {
		rj.write(w)
		return
	}
	if req.Method == methodInitialize {
		h.initialize(w, req)
		return
	}
	c, rj := h.connection(r)
	if rj != nil {
		rj.write(w)
		return
	}
	h.dispatch(w, r, c, req)
}

// dispatch answers every message other than initialize: accepted immediately,
// answered later on a stream. Which stream is fixed by the method.
func (h *handler) dispatch(w http.ResponseWriter, r *http.Request, c *conn, req jsonrpc2.Request) {
	switch req.Method {
	case methodNewSession:
		accepted(w)
		// The client has no session id yet, so this answer goes to the connection.
		c.send(connectionScope, h.newSession(c, req))
	case methodPrompt:
		id, rj := sessionOf(c, r, req)
		if rj != nil {
			rj.write(w)
			return
		}
		accepted(w)
		c.send(id, h.prompt(r.Context(), id, req))
	case methodCancel:
		if _, rj := sessionOf(c, r, req); rj != nil {
			rj.write(w)
			return
		}
		// A v1 notification: it carries no id to answer, and with no agent there is
		// no turn in flight to stop.
		accepted(w)
	default:
		accepted(w)
		if !req.Notif {
			c.send(connectionScope, failed(req.ID, jsonrpc2.CodeMethodNotFound, "method not found: "+req.Method))
		}
	}
}

func (h *handler) openStream(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "momo serves the streamable HTTP profile only", http.StatusNotImplemented)
		return
	}
	if !accepts(r.Header.Get("Accept"), sseMediaType) {
		http.Error(w, "expected Accept: "+sseMediaType, http.StatusNotAcceptable)
		return
	}
	c, rj := h.connection(r)
	if rj != nil {
		rj.write(w)
		return
	}
	scope, rj := scopeOf(c, r)
	if rj != nil {
		rj.write(w)
		return
	}
	s := newStream()
	if err := c.attach(scope, s); err != nil {
		// A scope already streaming is a conflict; anything else means the connection
		// was terminated while this request was being routed.
		status := http.StatusNotFound
		if errors.Is(err, errScopeTaken) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer c.detach(scope, s)
	s.serve(r.Context(), h.life.Done(), w)
}

func (h *handler) terminate(w http.ResponseWriter, r *http.Request) {
	c, rj := h.connection(r)
	if rj != nil {
		rj.write(w)
		return
	}
	h.conns.remove(r.Header.Get(connectionHeader))
	// The connection's sessions and streams go with it.
	c.close()
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) authorized(r *http.Request) bool {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) == 1
}

func (h *handler) connection(r *http.Request) (*conn, *reject) {
	id := r.Header.Get(connectionHeader)
	if id == "" {
		return nil, &reject{http.StatusBadRequest, connectionHeader + " is required"}
	}
	c := h.conns.lookup(id)
	if c == nil {
		return nil, &reject{http.StatusNotFound, "unknown connection"}
	}
	return c, nil
}

// scopeOf reads which stream a request belongs to: a session's when it names
// one, the connection's otherwise.
func scopeOf(c *conn, r *http.Request) (string, *reject) {
	id := r.Header.Get(sessionHeader)
	if id == "" {
		return connectionScope, nil
	}
	if !c.hasSession(id) {
		return "", &reject{http.StatusNotFound, "unknown session for this connection"}
	}
	return id, nil
}

// sessionOf is scopeOf for the methods that must name a session, and holds the
// two identities to the same session.
func sessionOf(c *conn, r *http.Request, req jsonrpc2.Request) (string, *reject) {
	if r.Header.Get(sessionHeader) == "" {
		return "", &reject{http.StatusBadRequest, sessionHeader + " is required"}
	}
	scope, rj := scopeOf(c, r)
	if rj != nil {
		return "", rj
	}
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(rawParams(req), &p); err == nil && p.SessionID != "" && p.SessionID != scope {
		return "", &reject{http.StatusBadRequest, "sessionId does not match " + sessionHeader}
	}
	return scope, nil
}

func accepted(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
	// This empty response is the whole answer to the POST, so it is flushed before
	// momo acts on the message rather than when the handler returns.
	_ = http.NewResponseController(w).Flush()
}

// accepts reports whether the header lists the media type. The transport makes
// naming it a MUST, so a wildcard does not stand in for it.
func accepts(header, want string) bool {
	for entry := range strings.SplitSeq(header, ",") {
		if baseMediaType(entry) == want {
			return true
		}
	}
	return false
}

func baseMediaType(v string) string {
	t, _, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return t
}
