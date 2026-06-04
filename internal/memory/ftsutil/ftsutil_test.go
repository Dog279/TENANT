package ftsutil_test

import (
	"strings"
	"testing"

	"tenant/internal/memory/ftsutil"
)

func TestSanitize_DropsStopWordsKeepsContent(t *testing.T) {
	got := ftsutil.Sanitize("what outdoor activities does the user enjoy")
	// "what","does","the","user" are stop words → gone. "outdoor",
	// "activities","enjoy" survive.
	for _, want := range []string{"outdoor", "activities", "enjoy"} {
		if !strings.Contains(got, want) {
			t.Errorf("content word %q dropped: %q", want, got)
		}
	}
	for _, gone := range []string{"what", "does", "the", "user"} {
		// token must not appear as a standalone OR term
		for _, tok := range strings.Split(got, " OR ") {
			if tok == gone {
				t.Errorf("stop word %q survived: %q", gone, got)
			}
		}
	}
}

func TestSanitize_ORJoins(t *testing.T) {
	got := ftsutil.Sanitize("golang concurrency patterns")
	if got != "golang OR concurrency OR patterns" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitize_StripsPunctuationNoFTSInjection(t *testing.T) {
	// FTS5 operators (: * " ( ) -) must never reach MATCH.
	got := ftsutil.Sanitize(`how do I configure: "tenant" (v2)* -prod?`)
	for _, bad := range []string{":", "*", `"`, "(", ")", "-"} {
		if strings.Contains(got, bad) {
			t.Fatalf("FTS operator %q leaked: %q", bad, got)
		}
	}
	if !strings.Contains(got, "configure") || !strings.Contains(got, "tenant") {
		t.Errorf("content lost: %q", got)
	}
}

func TestSanitize_AllStopWordsReturnsEmpty(t *testing.T) {
	if got := ftsutil.Sanitize("what is the and or to of"); got != "" {
		t.Fatalf("expected empty (all stop words), got %q", got)
	}
}

func TestSanitize_DropsSingleChars(t *testing.T) {
	got := ftsutil.Sanitize("a b go rust")
	if got != "go OR rust" {
		t.Fatalf("got %q, want 'go OR rust'", got)
	}
}

func TestSanitize_Empty(t *testing.T) {
	if got := ftsutil.Sanitize(""); got != "" {
		t.Fatalf("got %q", got)
	}
}
