package acp

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/core"
)

const token = "operator-token"

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`

type call struct {
	direction string
	message   core.Message
}

type capture struct {
	calls chan call
}

func (c capture) Received(_ context.Context, m core.Message) {
	c.calls <- call{direction: "received", message: m}
}

func (c capture) Sent(_ context.Context, m core.Message) {
	c.calls <- call{direction: "sent", message: m}
}

// with fills the channel's settings the way the operator's file would, leaving
// the path New defaults and standing for an absent key with a zero field.
func with(s settings) channel.Decoder {
	return func(v any) error {
		d := v.(*settings)
		s.Path = cmp.Or(s.Path, d.Path)
		*d = s
		return nil
	}
}

type peer struct {
	t     *testing.T
	url   string
	conns *connectionManager
	calls chan call
}

// defaultBudget mirrors the allowance config applies when the operator's file
// says nothing.
const defaultBudget = 128

func newChannel(t *testing.T, life context.Context, budget *channel.ConnectionBudget) (channel.Channel, chan call) {
	t.Helper()
	calls := make(chan call, 4)
	c, err := New(life, with(settings{Token: token}), capture{calls: calls}, budget)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	routes := c.Routes()
	if len(routes) != 1 {
		t.Fatalf("routes = %v, want exactly one", routes)
	}
	if routes[0].Path != "/acp" {
		t.Errorf("path = %q, want the default /acp", routes[0].Path)
	}
	return c, calls
}

func peerAt(t *testing.T, url string, c channel.Channel, calls chan call) *peer {
	return &peer{t: t, url: url, conns: c.Routes()[0].Handler.(*handler).conns, calls: calls}
}

func newPeer(t *testing.T, life context.Context) *peer {
	t.Helper()
	c, calls := newChannel(t, life, channel.NewConnectionBudget(defaultBudget))
	return served(t, c, calls)
}

func newPeerWithBudget(t *testing.T, budget *channel.ConnectionBudget) *peer {
	t.Helper()
	c, calls := newChannel(t, t.Context(), budget)
	return served(t, c, calls)
}

func served(t *testing.T, c channel.Channel, calls chan call) *peer {
	t.Helper()
	srv := httptest.NewServer(c.Routes()[0].Handler)
	t.Cleanup(srv.Close)
	return peerAt(t, srv.URL, c, calls)
}

func (p *peer) do(method, body string, hdr map[string]string) *http.Response {
	p.t.Helper()
	r, err := http.NewRequest(method, p.url, strings.NewReader(body))
	if err != nil {
		p.t.Fatalf("new request: %v", err)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		p.t.Fatalf("%s: %v", method, err)
	}
	p.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (p *peer) post(body string, hdr map[string]string) *http.Response {
	p.t.Helper()
	h := map[string]string{"Authorization": "Bearer " + token, "Content-Type": jsonMediaType}
	maps.Copy(h, hdr)
	return p.do(http.MethodPost, body, h)
}

func (p *peer) connections() int {
	p.conns.mu.Lock()
	defer p.conns.mu.Unlock()
	return len(p.conns.byID)
}

// initialize opens a connection and returns the id the transport issued, having
// checked it is reported in both the body and the response header.
func (p *peer) initialize() string {
	p.t.Helper()
	resp := p.post(initializeBody, nil)
	if resp.StatusCode != http.StatusOK {
		p.t.Fatalf("initialize = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var out struct {
		Result initializeResult `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		p.t.Fatalf("initialize body: %v", err)
	}
	if out.Result.ConnectionID == "" {
		p.t.Fatal("initialize answered no connectionId")
	}
	if got := resp.Header.Get(connectionHeader); got != out.Result.ConnectionID {
		p.t.Errorf("%s header = %q, want the id in the body %q", connectionHeader, got, out.Result.ConnectionID)
	}
	return out.Result.ConnectionID
}

type events struct {
	msgs  chan string
	ended chan struct{}
}

// stream opens an SSE stream and reads its messages in the background.
func (p *peer) stream(hdr map[string]string) *events {
	p.t.Helper()
	h := map[string]string{"Authorization": "Bearer " + token, "Accept": sseMediaType}
	maps.Copy(h, hdr)
	resp := p.do(http.MethodGet, "", h)
	if resp.StatusCode != http.StatusOK {
		p.t.Fatalf("GET = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	e := &events{msgs: make(chan string, 4), ended: make(chan struct{})}
	go func() {
		defer close(e.ended)
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if data, found := strings.CutPrefix(line, "data: "); found {
				e.msgs <- strings.TrimSpace(data)
			}
		}
	}()
	return e
}

func (e *events) next(t *testing.T) string {
	t.Helper()
	select {
	case m := <-e.msgs:
		return m
	case <-e.ended:
		t.Fatal("the stream ended before a message arrived")
	case <-time.After(2 * time.Second):
		t.Fatal("no message arrived on the stream")
	}
	return ""
}

// newSession creates a session, taking the answer off the connection-scoped
// stream the transport puts it on.
func (p *peer) newSession(connID string, connStream *events) string {
	p.t.Helper()
	body := `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`
	if got := p.post(body, map[string]string{connectionHeader: connID}).StatusCode; got != http.StatusAccepted {
		p.t.Fatalf("session/new = %d, want %d", got, http.StatusAccepted)
	}
	var out struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	msg := connStream.next(p.t)
	if err := json.Unmarshal([]byte(msg), &out); err != nil {
		p.t.Fatalf("session/new answer %q: %v", msg, err)
	}
	if out.Result.SessionID == "" {
		p.t.Fatalf("session/new answer %q carried no sessionId", msg)
	}
	return out.Result.SessionID
}

func promptBody(sessionID, text string) string {
	return `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"` + sessionID +
		`","prompt":[{"type":"text","text":"` + text + `"}]}}`
}

// session opens a connection and a session on it, and returns the session's id,
// the headers a request about that session needs, and the stream its answers
// arrive on.
func (p *peer) session() (string, map[string]string, *events) {
	p.t.Helper()
	id := p.initialize()
	connStream := p.stream(map[string]string{connectionHeader: id})
	sessionID := p.newSession(id, connStream)
	hdr := map[string]string{connectionHeader: id, sessionHeader: sessionID}
	return sessionID, hdr, p.stream(hdr)
}

// A prompt momo does not read is still a prompt: every block reaches the core as
// it arrived, so nothing is lost before there is an agent to hand it to.
func TestPromptCarriesEveryContentBlockToTheCore(t *testing.T) {
	p := newPeer(t, t.Context())
	sessionID, hdr, _ := p.session()

	body := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"` + sessionID +
		`","prompt":[{"type":"text","text":"look at this"},` +
		`{"type":"image","data":"aGk=","mimeType":"image/png"}]}}`
	if got := p.post(body, hdr).StatusCode; got != http.StatusAccepted {
		t.Fatalf("session/prompt = %d, want %d", got, http.StatusAccepted)
	}

	got := <-p.calls
	want := call{direction: "received", message: core.Message{Contact: sessionID, Content: []core.Block{
		core.Text("look at this"),
		{Type: "image", Data: "aGk=", MimeType: "image/png"},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("core saw %+v, want %+v", got, want)
	}
}

func TestPromptRefusesAnUnreadableBlockList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt string
	}{
		{name: "no blocks at all", prompt: `[]`},
		{name: "absent", prompt: ``},
		{name: "a block without a type", prompt: `[{"text":"hello"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPeer(t, t.Context())
			sessionID, hdr, sessionStream := p.session()

			params := `{"sessionId":"` + sessionID + `"`
			if tc.prompt != "" {
				params += `,"prompt":` + tc.prompt
			}
			body := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":` + params + `}}`
			if got := p.post(body, hdr).StatusCode; got != http.StatusAccepted {
				t.Fatalf("session/prompt = %d, want %d", got, http.StatusAccepted)
			}
			if answer := sessionStream.next(t); !strings.Contains(answer, `"code":-32602`) {
				t.Errorf("answer = %s, want invalid params", answer)
			}
			select {
			case got := <-p.calls:
				t.Errorf("core saw %+v, want no message", got)
			default:
			}
		})
	}
}

func TestInitializeAnswersTheNegotiatedVersionAndNoAuthMethods(t *testing.T) {
	p := newPeer(t, t.Context())
	resp := p.post(initializeBody, nil)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var out struct {
		Result initializeResult `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("initialize body: %v", err)
	}
	if out.Result.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %d, want %d", out.Result.ProtocolVersion, protocolVersion)
	}
	if !strings.Contains(string(body), `"authMethods":[]`) {
		t.Errorf("body = %s, want an empty authMethods list", body)
	}
}

func TestTokenIsRequiredOnEveryMethod(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		for name, auth := range map[string]string{"missing": "", "wrong": "Bearer not-the-token"} {
			t.Run(method+" "+name, func(t *testing.T) {
				p := newPeer(t, t.Context())
				hdr := map[string]string{"Content-Type": jsonMediaType, "Accept": sseMediaType}
				if auth != "" {
					hdr["Authorization"] = auth
				}
				resp := p.do(method, initializeBody, hdr)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("%s = %d, want %d", method, resp.StatusCode, http.StatusUnauthorized)
				}
				if got := p.connections(); got != 0 {
					t.Errorf("connections = %d, want none created", got)
				}
			})
		}
	}
}

func TestPostRejectsAnythingButJSON(t *testing.T) {
	p := newPeer(t, t.Context())
	for _, contentType := range []string{"", "text/plain"} {
		t.Run(contentType, func(t *testing.T) {
			hdr := map[string]string{"Authorization": "Bearer " + token}
			if contentType != "" {
				hdr["Content-Type"] = contentType
			}
			resp := p.do(http.MethodPost, initializeBody, hdr)
			if resp.StatusCode != http.StatusUnsupportedMediaType {
				t.Fatalf("POST = %d, want %d", resp.StatusCode, http.StatusUnsupportedMediaType)
			}
		})
	}
}

// A request that resolved its connection just before the connection was closed
// still holds it, and must not be able to open a stream nothing will ever close.
func TestAClosedConnectionAcceptsNothingMore(t *testing.T) {
	cm := newConnectionManager(defaultBudget)
	id, err := cm.create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := cm.lookup(id)
	// Closed the way DELETE closes it.
	cm.remove(id)
	c.close()
	if err := c.attach(connectionScope, newStream()); err == nil {
		t.Error("attach succeeded on a closed connection, want it refused")
	}
	if _, err := c.newSession(); err == nil {
		t.Error("newSession succeeded on a closed connection, want it refused")
	}
}

// fill opens as many connections as the manager holds, each listening or not as asked.
func fill(t *testing.T, cm *connectionManager, listening bool) {
	t.Helper()
	for range cm.maxRecords {
		id, err := cm.create()
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if listening {
			if err := cm.lookup(id).attach(connectionScope, newStream()); err != nil {
				t.Fatalf("attach: %v", err)
			}
		}
	}
}

func TestConnectionsAreCappedWhileClientsAreListening(t *testing.T) {
	cm := newConnectionManager(defaultBudget)
	fill(t, cm, true)
	if _, err := cm.create(); !errors.Is(err, errTooManyConns) {
		t.Fatalf("create past the cap = %v, want %v", err, errTooManyConns)
	}
}

// A client that goes away without sending DELETE must not hold its slot until
// momo restarts.
func TestAbandonedConnectionsMakeRoom(t *testing.T) {
	cm := newConnectionManager(defaultBudget)
	fill(t, cm, false)
	if _, err := cm.create(); err != nil {
		t.Fatalf("create past the cap = %v, want the abandoned connections dropped", err)
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if len(cm.byID) != 1 {
		t.Errorf("connections = %d, want only the new one", len(cm.byID))
	}
}

func TestSessionsAreCappedPerConnection(t *testing.T) {
	cm := newConnectionManager(defaultBudget)
	id, err := cm.create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := cm.lookup(id)
	for range maxSessionsPerConn {
		if _, err := c.newSession(); err != nil {
			t.Fatalf("newSession: %v", err)
		}
	}
	if _, err := c.newSession(); !errors.Is(err, errTooManySessions) {
		t.Fatalf("newSession past the cap = %v, want %v", err, errTooManySessions)
	}
}

func TestAnOversizedBodyIsAnsweredTooLarge(t *testing.T) {
	p := newPeer(t, t.Context())
	if got := p.post(strings.Repeat("x", maxBodyBytes+1), nil).StatusCode; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}
}

func TestStreamRequiresTheEventStreamAccept(t *testing.T) {
	p := newPeer(t, t.Context())
	id := p.initialize()
	for _, accept := range []string{"", "application/json", "*/*"} {
		t.Run(accept, func(t *testing.T) {
			hdr := map[string]string{"Authorization": "Bearer " + token, connectionHeader: id}
			if accept != "" {
				hdr["Accept"] = accept
			}
			resp := p.do(http.MethodGet, "", hdr)
			if resp.StatusCode != http.StatusNotAcceptable {
				t.Fatalf("GET = %d, want %d", resp.StatusCode, http.StatusNotAcceptable)
			}
		})
	}
}

func TestWebSocketUpgradeIsRefused(t *testing.T) {
	p := newPeer(t, t.Context())
	id := p.initialize()
	resp := p.do(http.MethodGet, "", map[string]string{
		"Authorization":  "Bearer " + token,
		"Accept":         sseMediaType,
		"Upgrade":        "websocket",
		connectionHeader: id,
	})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("GET with Upgrade = %d, want %d", resp.StatusCode, http.StatusNotImplemented)
	}
}

func TestIdentityHeadersAreChecked(t *testing.T) {
	p := newPeer(t, t.Context())
	id := p.initialize()
	connStream := p.stream(map[string]string{connectionHeader: id})
	sessionID := p.newSession(id, connStream)
	other := p.initialize()

	for _, tc := range []struct {
		name   string
		method string
		body   string
		hdr    map[string]string
		want   int
	}{
		{
			name: "POST without a connection id", method: http.MethodPost,
			body: promptBody(sessionID, "hello"), hdr: map[string]string{sessionHeader: sessionID},
			want: http.StatusBadRequest,
		},
		{
			name: "POST with an unknown connection id", method: http.MethodPost,
			body: promptBody(sessionID, "hello"),
			hdr:  map[string]string{connectionHeader: "no-such-connection", sessionHeader: sessionID},
			want: http.StatusNotFound,
		},
		{
			name: "session-scoped POST without a session id", method: http.MethodPost,
			body: promptBody(sessionID, "hello"), hdr: map[string]string{connectionHeader: id},
			want: http.StatusBadRequest,
		},
		{
			name: "POST with an unknown session id", method: http.MethodPost,
			body: promptBody(sessionID, "hello"),
			hdr:  map[string]string{connectionHeader: id, sessionHeader: "no-such-session"},
			want: http.StatusNotFound,
		},
		{
			name: "POST with a session of another connection", method: http.MethodPost,
			body: promptBody(sessionID, "hello"),
			hdr:  map[string]string{connectionHeader: other, sessionHeader: sessionID},
			want: http.StatusNotFound,
		},
		{
			name: "POST whose params name another session", method: http.MethodPost,
			body: promptBody("some-other-session", "hello"),
			hdr:  map[string]string{connectionHeader: id, sessionHeader: sessionID},
			want: http.StatusBadRequest,
		},
		{
			name: "GET without a connection id", method: http.MethodGet,
			hdr:  map[string]string{"Accept": sseMediaType},
			want: http.StatusBadRequest,
		},
		{
			name: "GET with an unknown connection id", method: http.MethodGet,
			hdr:  map[string]string{"Accept": sseMediaType, connectionHeader: "no-such-connection"},
			want: http.StatusNotFound,
		},
		{
			name: "GET with an unknown session id", method: http.MethodGet,
			hdr:  map[string]string{"Accept": sseMediaType, connectionHeader: id, sessionHeader: "no-such-session"},
			want: http.StatusNotFound,
		},
		{
			name: "DELETE without a connection id", method: http.MethodDelete,
			want: http.StatusBadRequest,
		},
		{
			name: "DELETE with an unknown connection id", method: http.MethodDelete,
			hdr:  map[string]string{connectionHeader: "no-such-connection"},
			want: http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{"Authorization": "Bearer " + token, "Content-Type": jsonMediaType}
			maps.Copy(hdr, tc.hdr)
			if got := p.do(tc.method, tc.body, hdr).StatusCode; got != tc.want {
				t.Fatalf("%s = %d, want %d", tc.method, got, tc.want)
			}
		})
	}
}

func TestBatchIsNotImplemented(t *testing.T) {
	p := newPeer(t, t.Context())
	resp := p.post(` [`+initializeBody+`]`, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("batch = %d, want %d", resp.StatusCode, http.StatusNotImplemented)
	}
}

func TestUnusableBodiesAreAnswered(t *testing.T) {
	p := newPeer(t, t.Context())
	for _, body := range []string{
		"",
		"not json at all",
		`{"jsonrpc":"1.0","method":"initialize"}`,
		`{"jsonrpc":"2.0","id":1}`,
		// An id momo cannot echo back unchanged is refused rather than answered
		// under a different id.
		`{"jsonrpc":"2.0","id":-1,"method":"initialize"}`,
	} {
		t.Run(body, func(t *testing.T) {
			if got := p.post(body, nil).StatusCode; got != http.StatusBadRequest {
				t.Fatalf("POST %q = %d, want %d", body, got, http.StatusBadRequest)
			}
		})
	}
}

func TestPromptReachesTheCoreAndIsAnsweredOnTheSessionStream(t *testing.T) {
	p := newPeer(t, t.Context())
	id := p.initialize()
	connStream := p.stream(map[string]string{connectionHeader: id})
	sessionID := p.newSession(id, connStream)
	sessionStream := p.stream(map[string]string{connectionHeader: id, sessionHeader: sessionID})

	hdr := map[string]string{connectionHeader: id, sessionHeader: sessionID}
	if got := p.post(promptBody(sessionID, "hello momo"), hdr).StatusCode; got != http.StatusAccepted {
		t.Fatalf("session/prompt = %d, want %d", got, http.StatusAccepted)
	}

	got := <-p.calls
	want := call{
		direction: "received",
		message:   core.Message{Contact: sessionID, Content: []core.Block{core.Text("hello momo")}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("core saw %+v, want %+v", got, want)
	}
	if answer := sessionStream.next(t); !strings.Contains(answer, `"stopReason":"end_turn"`) {
		t.Errorf("session/prompt answer = %s, want the turn to have ended", answer)
	}
	select {
	case msg := <-connStream.msgs:
		t.Errorf("the connection stream carried %s, want the prompt answer on the session stream", msg)
	default:
	}
}

func TestUnimplementedMethodIsAnsweredAndLeavesTheConnectionUsable(t *testing.T) {
	p := newPeer(t, t.Context())
	id := p.initialize()
	connStream := p.stream(map[string]string{connectionHeader: id})

	body := `{"jsonrpc":"2.0","id":9,"method":"session/list","params":{}}`
	if got := p.post(body, map[string]string{connectionHeader: id}).StatusCode; got != http.StatusAccepted {
		t.Fatalf("session/list = %d, want %d", got, http.StatusAccepted)
	}
	if answer := connStream.next(t); !strings.Contains(answer, `"code":-32601`) {
		t.Errorf("answer = %s, want a method-not-found error", answer)
	}
	// The connection still works.
	p.newSession(id, connStream)
}

func TestSessionNewRefusesMCPServers(t *testing.T) {
	p := newPeer(t, t.Context())
	id := p.initialize()
	connStream := p.stream(map[string]string{connectionHeader: id})

	body := `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp",` +
		`"mcpServers":[{"name":"tools","command":"/bin/tools","args":[]}]}}`
	if got := p.post(body, map[string]string{connectionHeader: id}).StatusCode; got != http.StatusAccepted {
		t.Fatalf("session/new = %d, want %d", got, http.StatusAccepted)
	}
	if answer := connStream.next(t); !strings.Contains(answer, `"code":-32602`) {
		t.Errorf("answer = %s, want invalid params", answer)
	}
}

func TestOneStreamPerScope(t *testing.T) {
	p := newPeer(t, t.Context())
	id := p.initialize()
	p.stream(map[string]string{connectionHeader: id})
	resp := p.do(http.MethodGet, "", map[string]string{
		"Authorization":  "Bearer " + token,
		"Accept":         sseMediaType,
		connectionHeader: id,
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second GET = %d, want %d", resp.StatusCode, http.StatusConflict)
	}
}

func TestDeleteReleasesSessionsAndClosesStreams(t *testing.T) {
	p := newPeer(t, t.Context())
	id := p.initialize()
	connStream := p.stream(map[string]string{connectionHeader: id})
	sessionID := p.newSession(id, connStream)
	sessionStream := p.stream(map[string]string{connectionHeader: id, sessionHeader: sessionID})

	hdr := map[string]string{"Authorization": "Bearer " + token, connectionHeader: id}
	if got := p.do(http.MethodDelete, "", hdr).StatusCode; got != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want %d", got, http.StatusNoContent)
	}
	for name, e := range map[string]*events{"connection": connStream, "session": sessionStream} {
		select {
		case <-e.ended:
		case <-time.After(2 * time.Second):
			t.Errorf("the %s stream stayed open after DELETE", name)
		}
	}
	if got := p.connections(); got != 0 {
		t.Errorf("connections = %d, want none left", got)
	}
	if got := p.do(http.MethodDelete, "", hdr).StatusCode; got != http.StatusNotFound {
		t.Errorf("DELETE of the same connection = %d, want %d", got, http.StatusNotFound)
	}
}

func TestTheConnectionCeilingFollowsMomosBudget(t *testing.T) {
	c, err := New(t.Context(), with(settings{Token: token}), nil, channel.NewConnectionBudget(512))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Routes()[0].Handler.(*handler).conns.maxRecords; got != 512 {
		t.Errorf("records = %d, want the budget's 512", got)
	}
}

func TestTokenIsRequired(t *testing.T) {
	if _, err := New(t.Context(), with(settings{}), nil, channel.NewConnectionBudget(defaultBudget)); err == nil {
		t.Fatal("New succeeded without a token, want an error")
	}
}

func TestAStreamPastMomosBudgetIsRefusedUntilOneEnds(t *testing.T) {
	p := newPeerWithBudget(t, channel.NewConnectionBudget(1))
	id := p.initialize()
	connStream := p.stream(map[string]string{connectionHeader: id})
	sessionID := p.newSession(id, connStream)

	resp := p.do(http.MethodGet, "", map[string]string{
		"Authorization":  "Bearer " + token,
		"Accept":         sseMediaType,
		connectionHeader: id,
		sessionHeader:    sessionID,
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second GET = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("no Retry-After, want the client told when to come back")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "connection limit") {
		t.Errorf("second GET said %q, want it to name momo's limit", body)
	}

	hdr := map[string]string{"Authorization": "Bearer " + token, connectionHeader: id}
	if got := p.do(http.MethodDelete, "", hdr).StatusCode; got != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want %d", got, http.StatusNoContent)
	}
	select {
	case <-connStream.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream stayed open after DELETE")
	}
	// stream fails the test unless the GET is answered with the stream.
	p.stream(map[string]string{connectionHeader: p.initialize()})
}

// A message momo answers needs an id to answer under: with none, the answer
// cannot be correlated by the client, and it used to go out under a made-up 0.
// session/cancel is v1's only client-to-server notification and stays accepted.
func TestAMethodMomoAnswersIsRefusedWithoutAnID(t *testing.T) {
	p := newPeer(t, t.Context())

	resp := p.post(`{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":1}}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("initialize without an id = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if got := resp.Header.Get(connectionHeader); got != "" {
		t.Errorf("%s = %q, want no connection issued", connectionHeader, got)
	}
	if got := p.connections(); got != 0 {
		t.Errorf("connections = %d, want none opened", got)
	}

	sessionID, hdr, _ := p.session()
	for _, tc := range []struct {
		method string
		body   string
		hdr    map[string]string
	}{
		{
			method: methodNewSession,
			body:   `{"jsonrpc":"2.0","method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`,
			hdr:    map[string]string{connectionHeader: hdr[connectionHeader]},
		},
		{
			method: methodPrompt,
			body: `{"jsonrpc":"2.0","method":"session/prompt","params":{"sessionId":"` + sessionID +
				`","prompt":[{"type":"text","text":"hello"}]}}`,
			hdr: hdr,
		},
	} {
		t.Run(tc.method, func(t *testing.T) {
			if got := p.post(tc.body, tc.hdr).StatusCode; got != http.StatusBadRequest {
				t.Errorf("%s without an id = %d, want %d", tc.method, got, http.StatusBadRequest)
			}
		})
	}

	cancel := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"` + sessionID + `"}}`
	if got := p.post(cancel, hdr).StatusCode; got != http.StatusAccepted {
		t.Errorf("session/cancel without an id = %d, want %d", got, http.StatusAccepted)
	}
}
