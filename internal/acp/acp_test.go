package acp

import (
	"encoding/json"
	"testing"
)

// A capability states support by presence, so the zero value must say nothing
// and a set one must say `{}`.
func TestSessionCapabilitiesStateSupportByPresence(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps SessionCapabilities
		want string
	}{
		{name: "none", caps: SessionCapabilities{}, want: `{}`},
		{
			name: "list and resume",
			caps: SessionCapabilities{List: &Capability{}, Resume: &Capability{}},
			want: `{"list":{},"resume":{}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.caps)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("got %s, want %s", encoded, tc.want)
			}
		})
	}
}
