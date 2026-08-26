package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/config"
)

type fixed struct {
	routes []channel.Route
}

func (f fixed) Routes() []channel.Route { return f.routes }

func instance(name string, paths ...string) channel.Instance {
	routes := make([]channel.Route, 0, len(paths))
	for _, p := range paths {
		routes = append(routes, channel.Route{Path: p, Handler: marker(p)})
	}
	return channel.Instance{Name: name, Channel: fixed{routes: routes}}
}

func marker(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	})
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func body(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d", path, w.Code, http.StatusOK)
	}
	return strings.TrimSpace(w.Body.String())
}

func TestMuxServesHealthAndChannelRoutes(t *testing.T) {
	mux, err := buildMux([]channel.Instance{instance("stub", "/stub/in")}, discard())
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	if got := body(t, mux, healthPath); got != "ok" {
		t.Errorf("%s answered %q, want \"ok\"", healthPath, got)
	}
	if got := body(t, mux, "/stub/in"); got != "/stub/in" {
		t.Errorf("/stub/in answered %q, want the channel's own handler", got)
	}
}

func TestMuxReportsAnUnusablePath(t *testing.T) {
	// http.ServeMux panics on these, so a typo in the configuration file would
	// take the process down instead of being reported.
	for _, path := range []string{"", "/respondio/{", "/respond io"} {
		t.Run(path, func(t *testing.T) {
			if _, err := buildMux([]channel.Instance{instance("stub", path)}, discard()); err == nil {
				t.Fatalf("buildMux(%q) succeeded, want an error naming the path", path)
			}
		})
	}
}

func TestMuxReportsAPathServedTwice(t *testing.T) {
	for _, tc := range []struct {
		name      string
		instances []channel.Instance
	}{
		{name: "same path twice in one channel", instances: []channel.Instance{instance("stub", "/in", "/in")}},
		{name: "path shared by two channels", instances: []channel.Instance{instance("a", "/in"), instance("b", "/in")}},
		{name: "path taken by health", instances: []channel.Instance{instance("a", healthPath)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A duplicate would panic inside http.ServeMux.
			if _, err := buildMux(tc.instances, discard()); err == nil {
				t.Fatal("buildMux succeeded, want an error naming the duplicate path")
			}
		})
	}
}

func loadConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "momo.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// agentBlock is the agent block every configuration a test serves with needs.
func agentBlock(t *testing.T, command string) string {
	t.Helper()
	return fmt.Sprintf("agent:\n  command: [%q]\n  data_dir: %q\n", command, t.TempDir())
}

// buildStub builds the stub ACP agent the agent package's tests use.
func buildStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stubagent")
	cmd := exec.Command("go", "build", "-o", path, "../../internal/agent/testdata/stubagent")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the stub agent: %v\n%s", err, out)
	}
	return path
}

// TestAMessageOnRespondioIsAnsweredByTheAgent drives the whole path: a signed
// webhook arrives, the agent subprocess runs the turn, and what it streamed
// leaves as the channel's delivery says. The stub agent's reply holds two
// paragraphs, so the separator decides how many messages the contact gets.
func TestAMessageOnRespondioIsAnsweredByTheAgent(t *testing.T) {
	for name, tc := range map[string]struct {
		delivery string
		want     []string
	}{
		"one message by default": {
			want: []string{`/contact/id:123/message {"message":{"text":"hello from\n\nthe stub agent","type":"text"}}`},
		},
		"one message per paragraph": {
			delivery: "    delivery:\n      separator: \"\\n\\n\"\n",
			want: []string{
				`/contact/id:123/message {"message":{"text":"hello from","type":"text"}}`,
				`/contact/id:123/message {"message":{"text":"the stub agent","type":"text"}}`,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			// One more than the turn delivers, so a message too many is recorded
			// instead of blocking the channel that sent it.
			sent := make(chan string, len(tc.want)+1)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("reading the API call: %v", err)
					return
				}
				sent <- r.URL.Path + " " + string(body)
				w.WriteHeader(http.StatusOK)
			}))
			defer api.Close()

			cfg := loadConfig(t, "listen: \"127.0.0.1:0\"\n"+agentBlock(t, buildStub(t))+
				fmt.Sprintf("channels:\n  respondio:\n    received_secret: secret\n    sent_secret: other\n    api_token: token\n    api_url: %q\n", api.URL)+
				tc.delivery)
			l, err := listen(cfg)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			go func() { _ = serve(ctx, discard(), cfg, l) }()

			post(t, l.Addr().String(), `{"event_type":"message.received","contact":{"id":123},"message":{"message":{"type":"text","text":"hi"}}}`)

			for _, want := range tc.want {
				select {
				case call := <-sent:
					if call != want {
						t.Fatalf("respond.io received %s, want %s", call, want)
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("no reply reached respond.io, want %s", want)
				}
			}
			select {
			case extra := <-sent:
				t.Fatalf("respond.io received %s as well, want %d call(s)", extra, len(tc.want))
			case <-time.After(time.Second):
			}
		})
	}
}

func post(t *testing.T, address, event string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(event))
	req, err := http.NewRequest(http.MethodPost, "http://"+address+"/respondio/received", strings.NewReader(event))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestAMessageForAHumanAssigneeIsRecordedInsteadOfAnswered drives the whole
// handover path: the conversation belongs to another respond.io user, so the
// message reaches the agent as a history record and the contact gets no reply.
func TestAMessageForAHumanAssigneeIsRecordedInsteadOfAnswered(t *testing.T) {
	sent := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()
	trace := filepath.Join(t.TempDir(), "sessions")
	t.Setenv("STUBAGENT_TRACE", trace)

	cfg := loadConfig(t, "listen: \"127.0.0.1:0\"\n"+agentBlock(t, buildStub(t))+
		"session_history:\n  user_command: \"/history-user\"\n  assistant_command: \"/history-assistant\"\n"+
		fmt.Sprintf("channels:\n  respondio:\n    received_secret: secret\n    sent_secret: other\n    api_token: token\n    api_url: %q\n    momo_user_id: 7\n", api.URL))
	l, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = serve(ctx, discard(), cfg, l) }()

	post(t, l.Addr().String(), `{"event_type":"message.received","contact":{"id":123,"assignee":{"id":9}},`+
		`"message":{"message":{"type":"text","text":"hi"}}}`)

	// The record runs one turn, so the agent opens a session for the conversation.
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(trace)
		if err == nil && strings.Contains(string(raw), "session/new") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never got the record; trace = %q, error = %v", raw, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case path := <-sent:
		t.Fatalf("respond.io was called on %s, want no reply to the contact", path)
	case <-time.After(time.Second):
	}
}

// TestServeStopsWithoutAnAgent pins the required block: momo answers a message
// with an agent, so a configuration with no agent never serves.
func TestServeStopsWithoutAnAgent(t *testing.T) {
	cfg := loadConfig(t, "listen: \"127.0.0.1:0\"\nchannels:\n  acp:\n    token: secret\n")
	l, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	if err := serve(context.Background(), discard(), cfg, l); err == nil {
		t.Fatal("serve succeeded, want an error naming the agent configuration")
	}
}

// TestServeStopsWhenAnAssigneeIdHasNoHistorySync pins that the two settings
// belong together: without the sync, a message for a human assignee would be
// dropped in silence.
func TestServeStopsWhenAnAssigneeIdHasNoHistorySync(t *testing.T) {
	cfg := loadConfig(t, "listen: \"127.0.0.1:0\"\n"+agentBlock(t, "/bin/true")+
		"channels:\n  respondio:\n    received_secret: a\n    sent_secret: b\n    api_token: t\n    momo_user_id: 7\n")
	l, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	if err := serve(context.Background(), discard(), cfg, l); err == nil {
		t.Fatal("serve succeeded, want an error naming the session history sync")
	}
}

// TestShutdownDoesNotWaitForAnOpenStream drives shutdown the way run does: the
// process cancels the context, and the channels are told through their lifetime.
func TestShutdownDoesNotWaitForAnOpenStream(t *testing.T) {
	cfg := loadConfig(t, "listen: \"127.0.0.1:0\"\nshutdown_timeout: 30s\n"+agentBlock(t, "/bin/true")+"channels:\n  acp:\n    token: secret\n")
	l, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- serve(ctx, discard(), cfg, l) }()

	endpoint := "http://" + l.Addr().String() + "/v1/acp"
	connID := initialize(t, endpoint)
	stream, err := open(endpoint, connID)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()

	stop()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for the open stream instead of closing it")
	}
}

func initialize(t *testing.T, endpoint string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`
	r, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	return resp.Header.Get("Acp-Connection-Id")
}

func open(endpoint, connID string) (*http.Response, error) {
	r, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Acp-Connection-Id", connID)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stream = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	return resp, nil
}

func TestListenerStopsAcceptingPastTheConfiguredMaximum(t *testing.T) {
	cfg := loadConfig(t, "listen: \"127.0.0.1:0\"\nmax_connections: 1\n")
	l, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: marker("ok"), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()

	held, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := &http.Client{
		Timeout:   300 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	if _, err := client.Get("http://" + l.Addr().String()); err == nil {
		t.Fatal("a second connection was served while the maximum was reached")
	}

	_ = held.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + l.Addr().String())
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the freed slot was never served: %v", err)
		}
	}
}

// TestAnOpenStreamOutlivesReadTimeout pins what read_timeout does not cut:
// net/http clears the read deadline before the handler runs, so a stream stays
// usable while a request's body is still bounded in time.
func TestAnOpenStreamOutlivesReadTimeout(t *testing.T) {
	cfg := loadConfig(t, "listen: \"127.0.0.1:0\"\nread_timeout: \"300ms\"\n"+agentBlock(t, "/bin/true")+"channels:\n  acp:\n    token: secret\n")
	l, err := listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = serve(ctx, discard(), cfg, l) }()

	endpoint := "http://" + l.Addr().String() + "/v1/acp"
	connID := initialize(t, endpoint)
	stream, err := open(endpoint, connID)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	frames := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stream.Body)
		for scanner.Scan() {
			if data, found := strings.CutPrefix(scanner.Text(), "data: "); found {
				frames <- data
				return
			}
		}
		close(frames)
	}()

	time.Sleep(time.Second)

	// session/new is answered on the connection-scoped stream, so a frame arriving
	// means the stream outlived three read timeouts.
	body := `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}`
	r, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Acp-Connection-Id", connID)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("session/new = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	select {
	case frame, open := <-frames:
		if !open {
			t.Fatal("the stream was closed before the response arrived")
		}
		if !strings.Contains(frame, "sessionId") {
			t.Fatalf("frame = %s, want the session/new result", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no response arrived on a stream older than read_timeout")
	}
}
