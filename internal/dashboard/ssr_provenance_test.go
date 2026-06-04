package dashboard

import (
	"strings"
	"testing"
	"time"
)

// The compaction-provenance page (TEN-104) renders "nothing compacted yet" when
// there's no summary, and the source range + summary + rehydrated turns when
// there is.
func TestSSR_CompactionProvenance(t *testing.T) {
	s, fm := memServer()

	// Nothing compacted yet.
	body := get(t, s, "/memory/provenance").Body.String()
	if !strings.Contains(body, "Nothing has been compacted") {
		t.Errorf("empty provenance should say nothing compacted yet:\n%s", body)
	}

	// With a summary → range + summary text + an original turn.
	fm.compProv = &CompactionProvenanceView{
		HasSummary: true,
		Summary:    "## Active Task\nship P4.6",
		MsgCount:   3,
		Origin:     "working",
		After:      time.Unix(1000, 0),
		Before:     time.Unix(2000, 0),
		Events:     []ProvenanceEventView{{When: time.Unix(1000, 0), Role: "user", Content: "the original question"}},
	}
	body = get(t, s, "/memory/provenance").Body.String()
	for _, want := range []string{"ship P4.6", "the original question", "3 turns", "Compaction"} {
		if !strings.Contains(body, want) {
			t.Errorf("provenance page missing %q:\n%s", want, body)
		}
	}
}
