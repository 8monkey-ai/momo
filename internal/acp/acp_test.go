package acp_test

import (
	"testing"

	"github.com/8monkey-ai/momo/internal/acp"
)

func TestProtocolVersionIsOne(t *testing.T) {
	if acp.ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1", acp.ProtocolVersion)
	}
}
