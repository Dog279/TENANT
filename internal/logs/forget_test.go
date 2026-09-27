package logs

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A forgotten session's lines are blanked in place: every other line, and a
// line another process appends while the file is open, survive; a dry run
// changes nothing.
func TestForgetLinesBlanksInPlace(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.Local)
	line := func(msg string, kv ...string) string {
		e := Entry{At: at, Source: "turns", Level: slog.LevelInfo, Msg: msg}
		for i := 0; i+1 < len(kv); i += 2 {
			e.Fields = append(e.Fields, Field{Key: kv[i], Value: kv[i+1]})
		}
		return e.Line() + "\n"
	}
	gone1 := line("tool call for 221B Baker Street", "session", "sess_a")
	keep := line("another session", "session", "sess_b")
	gone2 := line("model call", "trace", "run-1")
	plain := "free-form script output mentioning session=sess_a\n"
	path := filepath.Join(root, "turns", "2026-09-27.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := gone1 + keep + gone2 + plain
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Another process holds the file open for appending, as a running hub does.
	app, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	match := func(e Entry) bool { return e.Field("session") == "sess_a" || e.Field("trace") == "run-1" }

	if n, err := ForgetLines(root, match, false); err != nil || n != 2 {
		t.Fatalf("dry run = %d, %v; want 2", n, err)
	}
	if b, _ := os.ReadFile(path); string(b) != body {
		t.Fatal("a dry run changed the file")
	}
	if n, err := ForgetLines(root, match, true); err != nil || n != 2 {
		t.Fatalf("apply = %d, %v; want 2", n, err)
	}
	late := line("appended after the forget", "session", "sess_b")
	if _, err := app.WriteString(late); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat(" ", len(gone1)-1) + "\n" + keep + strings.Repeat(" ", len(gone2)-1) + "\n" + plain + late
	if string(b) != want {
		t.Fatalf("file after the forget:\n%q\nwant\n%q", b, want)
	}
	h, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.Start(t.Context())
	got, _ := h.Snapshot()
	var msgs []string
	for _, e := range got {
		msgs = append(msgs, e.Msg)
	}
	slices.Sort(msgs) // a free-form line is stamped when it is read
	if strings.Join(msgs, "|") != "another session|appended after the forget|"+strings.TrimSuffix(plain, "\n") {
		t.Errorf("a reader of the file sees %q", msgs)
	}
}
