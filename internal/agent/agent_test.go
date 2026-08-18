package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestTheNameOfAnIdentityIsFixed states one name as a literal. The name decides
// where a conversation's session lives, so a change to the readable part, to the
// digest, or to their order moves every conversation that exists to a directory
// with no session in it.
func TestTheNameOfAnIdentityIsFixed(t *testing.T) {
	if got := dirName("respondio:1"); got != "respondio-1-62f493009d38a7dd" {
		t.Fatalf("dirName = %q, want \"respondio-1-62f493009d38a7dd\"", got)
	}
}

func TestDirNameIsOneSafeSegment(t *testing.T) {
	for _, identity := range []string{"respondio:../etc", "acp:/absolute/path", "..", ".", "", "acp:.."} {
		got := dirName(identity)
		if got == "" || got == "." || got == ".." {
			t.Fatalf("dirName(%q) = %q, want a usable segment", identity, got)
		}
		if strings.ContainsRune(got, filepath.Separator) || strings.ContainsAny(got, `/\`) {
			t.Fatalf("dirName(%q) = %q, want no path separator", identity, got)
		}
	}
}

func TestDirNameIsBounded(t *testing.T) {
	got := dirName("respondio:" + strings.Repeat("x", 5000))
	if len(got) > 128 {
		t.Fatalf("len(dirName) = %d, want at most 128", len(got))
	}
}

func TestDifferentIdentitiesNeverShareAName(t *testing.T) {
	first, second := dirName("respondio:a/b"), dirName("respondio:a:b")
	if first == second {
		t.Fatalf("dirName collided: both %q", first)
	}
}

func TestLongIdentitiesThatShareAPrefixNeverShareAName(t *testing.T) {
	prefix := "respondio:" + strings.Repeat("y", 5000)
	if first, second := dirName(prefix+"1"), dirName(prefix+"2"); first == second {
		t.Fatalf("dirName collided: both %q", first)
	}
}
