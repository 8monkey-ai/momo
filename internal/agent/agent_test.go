package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

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
