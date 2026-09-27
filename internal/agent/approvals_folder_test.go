package agent

import (
	"path/filepath"
	"testing"

	"tenant/internal/logs"
)

// Approvals move into the Security log (docs/design/logging-v2.md §6.3),
// but the v1 approvals folder keeps being written for one more release, so
// tail -f and scripts reading it keep working while they move over (§13,
// Rollout; TEN-398). In the release after, drop this test and route these
// lines to security.
func TestApprovalLinesKeepTheApprovalsFolderForOneRelease(t *testing.T) {
	root := t.TempDir()
	h, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []Event{
		{Kind: EventApproval, Text: "os_exec: rm -rf build"},
		{Kind: EventApprovalPending, Approval: &ApprovalEvent{ID: "ap-1", Category: "exec", Action: "os_exec"}},
		{Kind: EventApprovalResolved, Approval: &ApprovalEvent{ID: "ap-1", Category: "exec", Action: "os_exec", Outcome: "approved"}},
	} {
		e, ok := EntryFor(ev)
		if !ok || e.Source != "approvals" {
			t.Fatalf("%s files under %q, want approvals", ev.Kind, e.Source)
		}
		h.Log(e)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(root, "approvals", "*.log")); len(files) != 1 {
		t.Fatalf("the approvals folder holds %v", files)
	}
}
