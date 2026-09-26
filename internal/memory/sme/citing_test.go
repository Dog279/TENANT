package sme

import (
	"context"
	"testing"

	"tenant/internal/memory/semantic"
)

func TestCitingFactFindsActiveSectionsOnly(t *testing.T) {
	ctx := context.Background()
	sem, err := semantic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer sem.Close()
	st, err := New(sem.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertSection(ctx, Section{AgentID: "main", Section: "Stack", Body: "Go", SourceFactIDs: []int64{12}}); err != nil {
		t.Fatal(err)
	}
	// Version 2 of Stack no longer cites 12; People cites 1 and 12.
	if _, err := st.UpsertSection(ctx, Section{AgentID: "main", Section: "Stack", Body: "Go and Rust", SourceFactIDs: []int64{3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertSection(ctx, Section{AgentID: "main", Section: "People", Body: "Ada", SourceFactIDs: []int64{1, 12}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.CitingFact(ctx, 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Section != "People" {
		t.Fatalf("sections citing 12 = %+v", got)
	}
	if got, _ := st.CitingFact(ctx, 2); len(got) != 0 {
		t.Fatalf("fact 2 is cited by nothing, got %+v", got)
	}
}
