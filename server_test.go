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

func TestTypingDelayCapped(t *testing.T) {
	if d := typingDelay(strings.Repeat("x", 100000), 30*time.Millisecond); d != maxTypingDelay {
		t.Fatalf("got %v, want cap %v", d, maxTypingDelay)
	}
}

func buildTestAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "testagent")
	cmd := exec.Command("go", "build", "-o", bin, "./testagent")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building testagent: %v\n%s", err, out)
	}
	return bin
}

// fakeRespond captures messages the server sends back to respond.io.
type fakeRespond struct {
	mu       sync.Mutex
	messages []string
	nextID   int64
}

func (f *fakeRespond) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.messages = append(f.messages, body.Message.Text)
		f.nextID++
		id := f.nextID
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]int64{"messageId": id})
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

func incomingText(contactID int64, text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"event_type": "message.received",
		"event_id":   "test-event",
		"contact":    map[string]any{"id": contactID, "firstName": "Test"},
		"message": map[string]any{
			"messageId": 42,
			"traffic":   "incoming",
			"message":   map[string]any{"type": "text", "text": text},
		},
	})
	return b
}

func TestEndToEnd(t *testing.T) {
	bin := buildTestAgent(t)
	fake := &fakeRespond{}
	respondSrv := httptest.NewServer(fake.handler())
	defer respondSrv.Close()

	cfg := config{
		respondToken:   "test-token",
		respondBaseURL: respondSrv.URL,
		agentCmd:       []string{bin},
		dataDir:        t.TempDir(),
	}
	srv := newServer(cfg)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

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

	// Second message from the same contact reuses the live session (same
	// harness process, no re-init) and still round-trips.
	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(incomingText(7, "again"))))
	msgs = fake.wait(t, 4)
	if msgs[2] != "You said: again" {
		t.Errorf("third message = %q", msgs[2])
	}

	// The sessionId must be persisted for session/load after restarts.
	b, err := os.ReadFile(filepath.Join(cfg.dataDir, "sessions.json"))
	if err != nil {
		t.Fatalf("session store not written: %v", err)
	}
	var store map[string]string
	json.Unmarshal(b, &store)
	if store["7"] != "test-session" {
		t.Errorf("session store = %v", store)
	}
}

func TestSignatureVerification(t *testing.T) {
	body := incomingText(1, "hi")
	key := "test-signing-key"

	fake := &fakeRespond{}
	respondSrv := httptest.NewServer(fake.handler())
	defer respondSrv.Close()

	cfg := config{
		respondToken:       "test-token",
		respondBaseURL:     respondSrv.URL,
		agentCmd:           []string{buildTestAgent(t)},
		dataDir:            t.TempDir(),
		incomingSigningKey: key,
	}
	srv := newServer(cfg)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

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

func TestOutgoingEchoFiltered(t *testing.T) {
	bin := buildTestAgent(t)
	fake := &fakeRespond{}
	respondSrv := httptest.NewServer(fake.handler())
	defer respondSrv.Close()

	cfg := config{
		respondToken:    "test-token",
		respondBaseURL:  respondSrv.URL,
		agentCmd:        []string{bin},
		dataDir:         t.TempDir(),
		outgoingCommand: "/operator-note",
	}
	srv := newServer(cfg)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	// Trigger a normal turn so the server records the messageIds it sent.
	http.Post(ts.URL+"/webhook", "application/json",
		strings.NewReader(string(incomingText(9, "hi"))))
	fake.wait(t, 2)

	// Echo of our own message (messageId 1) must be ignored: no new turn,
	// no new deliveries.
	echo, _ := json.Marshal(map[string]any{
		"event_type": "message.sent",
		"contact":    map[string]any{"id": 9},
		"message": map[string]any{
			"messageId": 1,
			"traffic":   "outgoing",
			"message":   map[string]any{"type": "text", "text": "You said: hi"},
		},
	})
	http.Post(ts.URL+"/webhook", "application/json", strings.NewReader(string(echo)))

	// An operator-sent message must reach the harness as a record-only turn:
	// the harness sees it, but nothing is delivered back to respond.io.
	operator, _ := json.Marshal(map[string]any{
		"event_type": "message.sent",
		"contact":    map[string]any{"id": 9},
		"message": map[string]any{
			"messageId": 555,
			"traffic":   "outgoing",
			"message":   map[string]any{"type": "text", "text": "operator reply"},
		},
	})
	http.Post(ts.URL+"/webhook", "application/json", strings.NewReader(string(operator)))

	time.Sleep(500 * time.Millisecond)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messages) != 2 {
		t.Errorf("expected no extra deliveries, got %q", fake.messages)
	}
}
