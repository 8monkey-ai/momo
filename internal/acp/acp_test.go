package acp

import (
	"encoding/json"
	"testing"
)

func TestSessionParamsCarryTheEmptyServerListVersionOneRequires(t *testing.T) {
	got, err := json.Marshal(Session("", "/srv/momo/conversation"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"cwd":"/srv/momo/conversation","mcpServers":[]}`
	if string(got) != want {
		t.Fatalf("session/new params = %s, want %s", got, want)
	}
}

func TestResumingParamsNameTheSession(t *testing.T) {
	got, err := json.Marshal(Session("session-1", "/srv/momo/conversation"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"sessionId":"session-1","cwd":"/srv/momo/conversation","mcpServers":[]}`
	if string(got) != want {
		t.Fatalf("session/resume params = %s, want %s", got, want)
	}
}

// TestACapabilityIsAdvertisedByPresence pins what a client reads off the
// handshake: an empty object means supported, and a missing field means it is not.
func TestACapabilityIsAdvertisedByPresence(t *testing.T) {
	var advertised AgentCapabilities
	if err := json.Unmarshal([]byte(`{"sessionCapabilities":{"resume":{}}}`), &advertised); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if advertised.SessionCapabilities.Resume == nil {
		t.Error("resume reads as unsupported, want supported")
	}
	if advertised.SessionCapabilities.List != nil {
		t.Error("list reads as supported, want unsupported")
	}
}

// TestOmittedCapabilitiesStayOffTheWire keeps momo's handshake honest: it
// advertises nothing it does not honour.
func TestOmittedCapabilitiesStayOffTheWire(t *testing.T) {
	got, err := json.Marshal(InitializeParams{ProtocolVersion: Version, ClientInfo: Info{Name: "momo"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"protocolVersion":1,"clientCapabilities":{},"clientInfo":{"name":"momo"}}`
	if string(got) != want {
		t.Fatalf("initialize params = %s, want %s", got, want)
	}
}
