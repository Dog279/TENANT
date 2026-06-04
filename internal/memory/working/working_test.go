package working_test

import (
	"sync"
	"testing"
	"time"

	"tenant/internal/memory/working"
)

func TestSet_AppendAndLen(t *testing.T) {
	s := working.New()
	if s.Len() != 0 {
		t.Fatalf("new Len = %d, want 0", s.Len())
	}
	s.Append(working.Message{Role: "user", Content: "hi"})
	s.Append(working.Message{Role: "assistant", Content: "hello"})
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
}

func TestSet_AppendAutofillsTimestamp(t *testing.T) {
	s := working.New()
	before := time.Now().UTC()
	s.Append(working.Message{Role: "user", Content: "hi"})
	m := s.Messages()[0]
	if m.Timestamp.Before(before) {
		t.Fatalf("Timestamp = %v, before = %v — not auto-filled", m.Timestamp, before)
	}
}

func TestSet_AppendPreservesExplicitTimestamp(t *testing.T) {
	s := working.New()
	ts := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	s.Append(working.Message{Role: "user", Content: "hi", Timestamp: ts})
	if !s.Messages()[0].Timestamp.Equal(ts) {
		t.Fatalf("Timestamp = %v, want %v", s.Messages()[0].Timestamp, ts)
	}
}

func TestSet_MessagesReturnsCopy(t *testing.T) {
	s := working.New()
	s.Append(working.Message{Role: "user", Content: "hi"})
	msgs := s.Messages()
	msgs[0].Content = "mutated"
	if s.Messages()[0].Content != "hi" {
		t.Fatal("Messages() did not return a defensive copy")
	}
}

func TestSet_Trim(t *testing.T) {
	s := working.New()
	for i := 0; i < 5; i++ {
		s.Append(working.Message{Role: "user", Content: "x"})
	}
	dropped := s.Trim(3)
	if dropped != 2 {
		t.Errorf("Trim returned %d, want 2", dropped)
	}
	if s.Len() != 3 {
		t.Errorf("Len after Trim = %d, want 3", s.Len())
	}
}

func TestSet_TrimNoChangeWhenAlreadyUnder(t *testing.T) {
	s := working.New()
	s.Append(working.Message{Role: "user", Content: "x"})
	if dropped := s.Trim(10); dropped != 0 {
		t.Fatalf("Trim returned %d, want 0", dropped)
	}
}

func TestSet_TrimKeepsMostRecent(t *testing.T) {
	s := working.New()
	for i, c := range []string{"a", "b", "c", "d", "e"} {
		_ = i
		s.Append(working.Message{Role: "user", Content: c})
	}
	s.Trim(2)
	got := s.Messages()
	if got[0].Content != "d" || got[1].Content != "e" {
		t.Fatalf("Trim dropped wrong messages: %v", []string{got[0].Content, got[1].Content})
	}
}

func TestSet_Reset(t *testing.T) {
	s := working.New()
	s.Append(working.Message{Role: "user", Content: "x"})
	s.Reset()
	if s.Len() != 0 {
		t.Fatalf("Len after Reset = %d, want 0", s.Len())
	}
}

func TestSet_ConcurrentAppendSafe(t *testing.T) {
	s := working.New()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Append(working.Message{Role: "user", Content: "x"})
		}()
	}
	wg.Wait()
	if s.Len() != 100 {
		t.Fatalf("Len = %d, want 100 (concurrent appends lost)", s.Len())
	}
}
