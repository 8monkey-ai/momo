package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestStderrIsLoggedOneRecordPerLine(t *testing.T) {
	var logged bytes.Buffer
	w := &stderrLog{log: slog.New(slog.NewTextHandler(&logged, nil))}
	for _, part := range []string{"first line\nsec", "ond line\r\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	records := strings.Count(logged.String(), "\n")
	if records != 2 {
		t.Errorf("records = %d, want 2:\n%s", records, logged.String())
	}
	if !strings.Contains(logged.String(), `line="second line"`) {
		t.Errorf("log = %s, want the line the two writes carried", logged.String())
	}
}
