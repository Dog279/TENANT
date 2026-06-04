package compress

import (
	"strings"
	"testing"
)

func TestExtractGoalHeader_BothSections(t *testing.T) {
	summary := `Some preamble prose with no headings.

## Active Task
Implement TEN-102: persistent goal header + trigger hysteresis.

## Resolved
- shipped P2

## Pending
- wire both Assemble sites
- add tests

## Key Facts
- budget is 13184 tokens`

	got := ExtractGoalHeader(summary)
	if got == "" {
		t.Fatal("expected a non-empty header")
	}
	if !strings.Contains(got, "TEN-102") {
		t.Errorf("active task missing: %q", got)
	}
	if !strings.Contains(got, "wire both Assemble sites") || !strings.Contains(got, "add tests") {
		t.Errorf("pending items missing: %q", got)
	}
	// Sibling sections must NOT leak in.
	if strings.Contains(got, "shipped P2") || strings.Contains(got, "13184") {
		t.Errorf("Resolved/Key Facts leaked into the header: %q", got)
	}
	// Reference framing present (trust boundary).
	if !strings.Contains(strings.ToLower(got), "not a new instruction") {
		t.Errorf("header must be reference-framed: %q", got)
	}
}

// The CRITICAL leak case: Artifacts omitted so ## Pending directly abuts the
// ## Verbatim allowlist. The verbatim values must NOT bleed into the header.
func TestExtractGoalHeader_VerbatimHardStop(t *testing.T) {
	summary := `## Active Task
Fix the parser.

## Pending
- one open item

## Verbatim (exact values — do not paraphrase)
- API_KEY=sk-secret-do-not-leak
- /etc/passwd
- commit a1b2c3d4`

	got := ExtractGoalHeader(summary)
	if !strings.Contains(got, "one open item") {
		t.Errorf("pending missing: %q", got)
	}
	if strings.Contains(got, "sk-secret-do-not-leak") || strings.Contains(got, "/etc/passwd") || strings.Contains(got, "a1b2c3d4") {
		t.Fatalf("Verbatim allowlist leaked into the goal header: %q", got)
	}
}

func TestExtractGoalHeader_ActiveOnly(t *testing.T) {
	got := ExtractGoalHeader("## Active Task\nShip it.\n")
	if !strings.Contains(got, "Ship it.") {
		t.Errorf("active task missing: %q", got)
	}
	if strings.Contains(got, "**Pending:**") {
		t.Errorf("should not render an empty Pending: %q", got)
	}
}

func TestExtractGoalHeader_NoneReturnsEmpty(t *testing.T) {
	for _, in := range []string{
		"",
		"just prose, no headings at all",
		"## Resolved\n- only resolved stuff\n## Key Facts\n- nothing actionable",
		"## Active Task\n\n## Pending\n", // headings present but empty bodies
	} {
		if got := ExtractGoalHeader(in); got != "" {
			t.Errorf("ExtractGoalHeader(%q) = %q, want empty", in, got)
		}
	}
}

// Tolerant of heading-level / case / trailing-punctuation variance from small
// summarizer models.
func TestExtractGoalHeader_HeadingVariants(t *testing.T) {
	summary := "### active task:\nlowercase h3 heading\n\n### PENDING\n- still found"
	got := ExtractGoalHeader(summary)
	if !strings.Contains(got, "lowercase h3 heading") {
		t.Errorf("should match ### + lowercase + trailing colon: %q", got)
	}
	if !strings.Contains(got, "still found") {
		t.Errorf("should match uppercase PENDING: %q", got)
	}
}

func TestExtractGoalHeader_RuneCap(t *testing.T) {
	big := strings.Repeat("x", 5000)
	got := ExtractGoalHeader("## Active Task\n" + big)
	if n := len([]rune(got)); n > goalHeaderMaxRunes+1 { // +1 for the ellipsis
		t.Errorf("header not capped: %d runes", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a capped header should end with an ellipsis: %q", got[len(got)-10:])
	}
}
