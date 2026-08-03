package channel

import (
	"errors"
	"testing"

	"github.com/turisanapo/momo-with-pi-5/internal/core"
)

func noSettings(any) error { return nil }

func stub(name string) Factory {
	return func(Decoder, core.Handler) (Channel, error) {
		return fixed{routes: []Route{{Path: "/" + name}}}, nil
	}
}

type fixed struct {
	routes []Route
}

func (f fixed) Routes() []Route { return f.routes }

func TestBuildsRegisteredChannelsInAStableOrder(t *testing.T) {
	Register("stub-b", stub("b"))
	Register("stub-a", stub("a"))

	got, err := Build(map[string]Decoder{"stub-b": noSettings, "stub-a": noSettings}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) != 2 || got[0].Name != "stub-a" || got[1].Name != "stub-b" {
		t.Fatalf("instances = %+v, want stub-a then stub-b", got)
	}
}

func TestBuildRejectsUnconfiguredChannelName(t *testing.T) {
	if _, err := Build(map[string]Decoder{"telegran": noSettings}, nil); err == nil {
		t.Fatal("Build succeeded, want an error naming the unknown channel")
	}
}

func TestBuildReportsWhichChannelFailed(t *testing.T) {
	broken := errors.New("missing signing key")
	Register("stub-broken", func(Decoder, core.Handler) (Channel, error) { return nil, broken })

	_, err := Build(map[string]Decoder{"stub-broken": noSettings}, nil)
	if !errors.Is(err, broken) {
		t.Fatalf("error = %v, want it to wrap %v", err, broken)
	}
}
