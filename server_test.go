package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	}, 0)
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
	tn := newTurn(func(s string) error { got = append(got, s); return nil }, 0)
	tn.addChunk("done\n\npartial tail")
	tn.finish(false)
	if len(got) != 1 || got[0] != "done" {
		t.Fatalf("got %q, want [done]", got)
	}
}

func TestVideoURLPattern(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"https://cdn.example.com/clips/demo.mp4", "https://cdn.example.com/clips/demo.mp4"},
		{"Here you go: https://cdn.example.com/Demo.MP4 enjoy!", "https://cdn.example.com/Demo.MP4"},
		{"http://any-host.io/v.webm", "http://any-host.io/v.webm"},
		{"https://x.io/a.mov", "https://x.io/a.mov"},
		{"see example.com/demo.mp4", ""}, // no scheme
		{"plain text without links", ""},
		{"https://x.io/doc.pdf", ""},
	}
	for _, tt := range tests {
		if got := videoURLPattern.FindString(tt.text); got != tt.want {
			t.Errorf("FindString(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestTypingDelayPerWord(t *testing.T) {
	if d := typingDelay("three word reply", time.Second); d != 3*time.Second {
		t.Fatalf("got %v, want 3s", d)
	}
}

// testAgentBin compiles the stub harness once for the whole test run.
var testAgentBin = sync.OnceValue(func() string {
	bin := filepath.Join(os.TempDir(), "agent-server-testagent")
	if out, err := exec.Command("go", "build", "-o", bin, "./testagent").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("building testagent: %v\n%s", err, out))
	}
	return bin
})

// fakeRespond captures messages the server sends back to respond.io. Text
// messages are recorded as their text, attachments as "type:url".
type fakeRespond struct {
	mu       sync.Mutex
	messages []string
}

func (f *fakeRespond) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message messageContent `json:"message"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		msg := body.Message.Text
		if a := body.Message.Attachment; a != nil {
			msg = a.Type + ":" + a.URL
		}
		f.mu.Lock()
		f.messages = append(f.messages, msg)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]int64{"messageId": 1})
	})
}

func (f *fakeRespond) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.messages) >= n {
			out := append([]string(nil), f.messages...)
			f.mu.Unlock()
			return out
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("timed out waiting for %d messages, got %q", n, f.messages)
	return nil
}

// setupServer starts a fake respond.io backend and the agent server wired to
// it, filling in the config fields every test shares. Test-specific fields
// (signing keys, aiAssigneeID) come in via cfg.
func setupServer(t *testing.T, cfg config) (*fakeRespond, *httptest.Server, config) {
	t.Helper()
	fake := &fakeRespond{}
	respondSrv := httptest.NewServer(fake.handler())
	t.Cleanup(respondSrv.Close)
	cfg.apiToken = "test-token"
	cfg.apiBaseURL = respondSrv.URL
	cfg.agentCmd = testAgentBin()
	cfg.dataDir = t.TempDir()
	ts := httptest.NewServer(newServer(cfg).routes())
	t.Cleanup(ts.Close)
	return fake, ts, cfg
}

// waitForDir blocks until path exists; a contact dir appearing is the
// observable side effect of a harness turn (including record-only turns).
func waitForDir(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func webhookBody(eventType string, contactID, messageID, assigneeID int64, text string) []byte {
	c := map[string]any{"id": contactID}
	if assigneeID != 0 {
		c["assignee"] = map[string]any{"id": assigneeID}
	}
	b, _ := json.Marshal(map[string]any{
		"event_type": eventType,
		"contact":    c,
		"message": map[string]any{
			"messageId": messageID,
			"message":   map[string]any{"type": "text", "text": text},
		},
	})
	return b
}

func incomingText(contactID int64, text string) []byte {
	return webhookBody("message.received", contactID, 42, 0, text)
}

func TestEndToEnd(t *testing.T) {
	fake, ts, cfg := setupServer(t, config{})

	resp, err := http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(incomingText(7, "hello agent"))))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook status %d", resp.StatusCode)
	}

	msgs := fake.wait(t, 2)
	if msgs[0] != "You said: hello agent" {
		t.Errorf("first message = %q", msgs[0])
	}
	if msgs[1] != "Second paragraph." {
		t.Errorf("second message = %q", msgs[1])
	}

	// The harness is recycled after each turn, so a second message respawns it
	// and rediscovers the prior session via session/list + session/load. It
	// still round-trips.
	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(incomingText(7, "again"))))
	msgs = fake.wait(t, 4)
	if msgs[2] != "You said: again" {
		t.Errorf("third message = %q", msgs[2])
	}

	// No persistent session store: discovery is stateless via session/list.
	if _, err := os.Stat(filepath.Join(cfg.dataDir, "sessions.json")); !os.IsNotExist(err) {
		t.Errorf("sessions.json should not exist; discovery is via session/list")
	}
}

// The second turn must resume the session created by the first (the testagent
// persists sessions to disk so session/list finds them across the per-turn
// recycle), not spawn an unbounded set of new sessions.
func TestSessionDiscoveryAcrossTurns(t *testing.T) {
	fake, ts, cfg := setupServer(t, config{})

	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(incomingText(21, "first"))))
	fake.wait(t, 2)
	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(incomingText(21, "second"))))
	fake.wait(t, 4)

	b, err := os.ReadFile(filepath.Join(cfg.dataDir, "21", ".testagent-sessions.json"))
	if err != nil {
		t.Fatalf("reading testagent sessions: %v", err)
	}
	var recs []struct {
		SessionId string `json:"sessionId"`
	}
	json.Unmarshal(b, &recs)
	if len(recs) != 1 {
		t.Errorf("expected exactly one session created (rest resumed via load), got %d: %v", len(recs), recs)
	}
}

// A reply paragraph containing a video URL is delivered as a video attachment;
// paragraphs without one stay plain text.
func TestVideoReplyDeliveredAsAttachment(t *testing.T) {
	fake, ts, _ := setupServer(t, config{})

	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(incomingText(8, "watch https://cdn.example.com/demo.mp4 now"))))

	msgs := fake.wait(t, 2)
	if msgs[0] != "video:https://cdn.example.com/demo.mp4" {
		t.Errorf("first message = %q, want video attachment", msgs[0])
	}
	if msgs[1] != "Second paragraph." {
		t.Errorf("second message = %q", msgs[1])
	}
}

func TestSignatureVerification(t *testing.T) {
	body := incomingText(1, "hi")
	key := "test-signing-key"
	_, ts, _ := setupServer(t, config{incomingSigningKey: key})

	post := func(sig string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook", strings.NewReader(string(body)))
		if sig != "" {
			req.Header.Set("X-Webhook-Signature", sig)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(""); code != http.StatusUnauthorized {
		t.Errorf("missing signature: got %d, want 401", code)
	}
	if code := post("bm90LXRoZS1zaWduYXR1cmU="); code != http.StatusUnauthorized {
		t.Errorf("wrong signature: got %d, want 401", code)
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	valid := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if code := post(valid); code != http.StatusOK {
		t.Errorf("valid signature: got %d, want 200", code)
	}
}

func TestAssigneeGate(t *testing.T) {
	fake, ts, _ := setupServer(t, config{aiAssigneeID: 471663})

	// Assigned to someone else: recorded via the slash command (record-only
	// turn), nothing delivered back.
	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(webhookBody("message.received", 11, 42, 999, "human is handling"))))
	time.Sleep(500 * time.Millisecond)
	fake.mu.Lock()
	if len(fake.messages) != 0 {
		t.Errorf("expected no deliveries for other assignee, got %q", fake.messages)
	}
	fake.mu.Unlock()

	// Assigned to the AI user: replied to normally.
	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(webhookBody("message.received", 11, 42, 471663, "for the ai"))))
	msgs := fake.wait(t, 2)
	if msgs[0] != "You said: for the ai" {
		t.Errorf("ai-assigned message = %q", msgs[0])
	}

	// Unassigned — an object of nulls on the wire: also replied to.
	body, _ := json.Marshal(map[string]any{
		"event_type": "message.received",
		"contact": map[string]any{
			"id":       11,
			"assignee": map[string]any{"id": nil, "firstName": nil, "lastName": nil, "email": nil},
		},
		"message": map[string]any{
			"messageId": 43,
			"message":   map[string]any{"type": "text", "text": "unassigned"},
		},
	})
	http.Post(ts.URL+"/webhook", "application/json", strings.NewReader(string(body)))
	msgs = fake.wait(t, 4)
	if msgs[2] != "You said: unassigned" {
		t.Errorf("unassigned message = %q", msgs[2])
	}
}

// Outgoing messages are only recorded when a human owns the conversation;
// everywhere else they are the agent's own replies and must be skipped to
// avoid echo loops.
func TestOutgoingRecordedOnlyWhenAssignedToHuman(t *testing.T) {
	fake, ts, cfg := setupServer(t, config{aiAssigneeID: 471663})

	// Assigned to the AI or unassigned: no harness turn at all — no contact
	// dir appears.
	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(webhookBody("message.sent", 13, 777, 471663, "our own reply"))))
	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(webhookBody("message.sent", 13, 779, 0, "unassigned reply"))))
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(cfg.dataDir, "13")); !os.IsNotExist(err) {
		t.Errorf("AI-assigned or unassigned outgoing message spawned a harness turn")
	}

	// Assigned to a human: recorded as a record-only turn (harness spawns,
	// nothing delivered back).
	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(webhookBody("message.sent", 13, 778, 999, "human operator reply"))))
	waitForDir(t, filepath.Join(cfg.dataDir, "13"))
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messages) != 0 {
		t.Errorf("record-only turn delivered messages: %q", fake.messages)
	}
}

// Without RESPOND_AI_ASSIGNEE_ID the agent handles every conversation, so
// every outgoing message is its own reply and none may be recorded.
func TestOutgoingSkippedWithoutAssigneeGate(t *testing.T) {
	_, ts, cfg := setupServer(t, config{})

	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(webhookBody("message.sent", 14, 780, 999, "reply"))))
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(cfg.dataDir, "14")); !os.IsNotExist(err) {
		t.Errorf("outgoing message without assignee gate spawned a harness turn")
	}
}
