package semantic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"tenant/internal/logs/event"
	"tenant/internal/memory/semantic"
)

type recorder struct{ events []*event.Event }

func (r *recorder) Emit(_ context.Context, e *event.Event) { r.events = append(r.events, e) }
func (r *recorder) Audit(ctx context.Context, e *event.Event) error {
	r.Emit(ctx, e)
	return nil
}

// Every change to a fact is a memory event carrying its fact_id; the text is
// kept only as a sensitive field. An in-memory store writes none.
func TestFactChangesAreEvents(t *testing.T) {
	r := &recorder{}
	event.Install(r)
	t.Cleanup(func() { event.Install(nil) })
	ctx := context.Background()
	s := mkStore(t)
	add := func(text string) int64 {
		id, err := s.Insert(ctx, &semantic.Fact{AgentID: "a", Fact: text, EmbedderID: "e", Embedding: []float32{1, 0},
			SourceRunIDs: []string{"run-1"}, OriginJob: "distill"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b := add("user lives in Lisbon"), add("user lives in Porto")
	if err := s.Supersede(ctx, a, b); err != nil {
		t.Fatal(err)
	}
	if err := s.Tombstone(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(ctx, b); err != nil {
		t.Fatal(err)
	}
	c, err := s.Edit(ctx, b, "user lives in Braga", "edit", "e", []float32{0, 1})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, e := range r.events {
		got = append(got, e.Msg)
	}
	want := []string{
		fmt.Sprintf("Fact %d added by distill", a), fmt.Sprintf("Fact %d added by distill", b),
		fmt.Sprintf("Fact %d superseded by fact %d", a, b),
		fmt.Sprintf("Fact %d removed", b), fmt.Sprintf("Fact %d restored", b),
		fmt.Sprintf("Fact %d added by edit", c), fmt.Sprintf("Fact %d superseded by fact %d", b, c),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
	for _, f := range r.events[0].Data {
		switch f.Key {
		case "fact":
			if !f.Sensitive || f.Value != "user lives in Lisbon" {
				t.Errorf("the fact's text must be kept as sensitive: %+v", f)
			}
		case "source_runs":
			if string(f.Value.(json.RawMessage)) != `["run-1"]` {
				t.Errorf("source_runs = %v", f.Value)
			}
		}
	}

	r.events = nil
	quiet, err := semantic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	if _, err := quiet.Insert(ctx, &semantic.Fact{AgentID: "a", Fact: "x", EmbedderID: "e", Embedding: []float32{1}}); err != nil {
		t.Fatal(err)
	}
	if len(r.events) != 0 {
		t.Fatalf("an in-memory store wrote %d events", len(r.events))
	}
}
