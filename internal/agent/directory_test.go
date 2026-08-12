package agent

import "testing"

func TestDirectoryNameAcceptsAnIdentityWithUnsafeCharacters(t *testing.T) {
	if got := directoryName("respondio:123"); got != "respondio_123-094e7ae2" {
		t.Errorf("directoryName = %q, want %q", got, "respondio_123-094e7ae2")
	}
}

func TestDirectoryNameKeepsIdentitiesThatDifferOnlyInUnsafeCharactersApart(t *testing.T) {
	if got := directoryName("a:b"); got != "a_b-6783a31e" {
		t.Errorf("directoryName(\"a:b\") = %q, want %q", got, "a_b-6783a31e")
	}
	if got := directoryName("a/b"); got != "a_b-c14cddc0" {
		t.Errorf("directoryName(\"a/b\") = %q, want %q", got, "a_b-c14cddc0")
	}
}

func TestDirectoryNameIsStableForOneIdentity(t *testing.T) {
	for range 3 {
		if got := directoryName("acp:9e0c2f"); got != "acp_9e0c2f-40380fff" {
			t.Errorf("directoryName = %q, want %q", got, "acp_9e0c2f-40380fff")
		}
	}
}
