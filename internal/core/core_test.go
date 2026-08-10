package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTextOfJoinsTheBlocksThatCarryText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks []ContentBlock
		want   string
	}{
		{name: "no blocks", want: ""},
		{name: "one text block", blocks: Text("hello"), want: "hello"},
		{
			name:   "two text blocks",
			blocks: []ContentBlock{{Type: "text", Text: "hello"}, {Type: "text", Text: "there"}},
			want:   "hello there",
		},
		{
			name:   "a block without text",
			blocks: []ContentBlock{{Type: "text", Text: "hello"}, {Type: "audio", Data: "AAAA"}},
			want:   "hello",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TextOf(tc.blocks); got != tc.want {
				t.Fatalf("TextOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEchoRepliesWithTheMessageItAnswers(t *testing.T) {
	var got []ContentBlock
	m := Message{Contact: "12345", Content: Text("hello")}
	EchoHandler{Log: discard()}.Received(context.Background(), m,
		func(_ context.Context, content []ContentBlock) error {
			got = content
			return nil
		})
	if !reflect.DeepEqual(got, m.Content) {
		t.Fatalf("replied with %+v, want %+v", got, m.Content)
	}
}

func TestEchoSurvivesAFailedReply(t *testing.T) {
	EchoHandler{Log: discard()}.Received(context.Background(), Message{Contact: "12345"},
		func(context.Context, []ContentBlock) error { return errors.New("the channel refused it") })
}
