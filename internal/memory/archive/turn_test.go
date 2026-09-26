package archive

import (
	"os"
	"testing"
	"time"
)

func TestTurnReturnsOneTurnOfItsSession(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	add := func(sess, role, content string, d time.Duration) {
		t.Helper()
		if err := w.Append(Event{Timestamp: at.Add(d), AgentID: "main", SessionID: sess, Role: role, Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	add("s1", "user", "hi", 0)
	add("s1", "assistant", "hello", time.Second)
	add("s1", "user", "what is my address", time.Minute)
	add("s1", "tool", "looked it up", time.Minute+time.Second)
	add("s1", "assistant", "221B Baker Street", time.Minute+2*time.Second)
	add("s2", "user", "what is my address", time.Minute) // same words, other session
	add("s1", "user", "thanks", 2*time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	r := NewReader(dir)
	got, err := r.Turn("s1", "what is my address", at.Add(time.Minute+3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Content != "what is my address" || got[2].Content != "221B Baker Street" {
		t.Fatalf("turn = %+v", got)
	}
	// An episode recorded long before the message cannot be that turn.
	if got, _ := r.Turn("s1", "what is my address", at.Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("matched a message sent after the episode: %+v", got)
	}
	if got, _ := r.Turn("s1", "never said", at.Add(time.Hour)); len(got) != 0 {
		t.Fatal("matched text that was never said")
	}
	if got, _ := r.Turn("", "hi", at); got != nil {
		t.Fatal("an episode without a session matched")
	}
}

// The same words sent twice: an episode is the latest sending before it.
func TestTurnPicksTheLatestSendingOfRepeatedWords(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for _, ev := range []Event{
		{Timestamp: at, SessionID: "s", Role: "user", Content: "status?"},
		{Timestamp: at.Add(time.Second), SessionID: "s", Role: "assistant", Content: "all green"},
		{Timestamp: at.Add(time.Hour), SessionID: "s", Role: "user", Content: "status?"},
		{Timestamp: at.Add(time.Hour + time.Second), SessionID: "s", Role: "assistant", Content: "one alert"},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	// A malformed line in the file doesn't hide the turns around it.
	f, err := os.OpenFile(PathFor(dir, at, "s"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r := NewReader(dir)
	got, err := r.Turn("s", "status?", at.Add(time.Hour+5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Content != "one alert" {
		t.Fatalf("the later turn = %+v", got)
	}
	if got, _ := r.Turn("s", "status?", at.Add(5*time.Second)); len(got) != 2 || got[1].Content != "all green" {
		t.Fatalf("the earlier turn = %+v", got)
	}
}
