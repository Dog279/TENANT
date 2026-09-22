package main

import "testing"

// parseExtButtonID is the filesystem-safety gate for ext-approve buttons: a
// crafted custom_id must never yield a namespace or id that can escape
// <dataDir>/approvals/<ns>. These tables pin the accepted shapes and the
// rejections that matter (traversal, separators, empty parts, wrong prefix).
func TestParseExtButtonID(t *testing.T) {
	valid := []struct {
		custom  string
		verdict string
		ns, id  string
	}{
		{"ext-approve:qi:QI-1", "approve", "qi", "QI-1"},
		{"ext-deny:qi:QI-12", "deny", "qi", "QI-12"},
		{"ext-approve:deploy-bot:release_42", "approve", "deploy-bot", "release_42"},
		{"ext-approve:a:B.2", "approve", "a", "B.2"},
	}
	for _, tc := range valid {
		verdict, ns, id, ok := parseExtButtonID(tc.custom)
		if !ok || verdict != tc.verdict || ns != tc.ns || id != tc.id {
			t.Errorf("parseExtButtonID(%q) = (%q,%q,%q,%v), want (%q,%q,%q,true)",
				tc.custom, verdict, ns, id, ok, tc.verdict, tc.ns, tc.id)
		}
	}

	invalid := []string{
		"",
		"approve:nonce",            // TEN-117 namespace, not ours
		"qi-approve:QI-1",          // pre-generalization id shape
		"ext-approve:qi",           // missing id
		"ext-approve::QI-1",        // empty namespace
		"ext-approve:qi:",          // empty id
		"ext-approve:QI:QI-1",      // uppercase namespace
		"ext-approve:qi:../escape", // traversal in id
		"ext-approve:../..:x",      // traversal in namespace
		"ext-approve:qi:a\\b",      // path separator in id
		"ext-approve:qi:a/b",       // path separator in id
		"ext-approve:qi:" + string(make([]byte, 70)), // over-length id
		"ext-nope:qi:QI-1",                           // unknown verb
	}
	for _, custom := range invalid {
		if _, _, _, ok := parseExtButtonID(custom); ok {
			t.Errorf("parseExtButtonID(%q) accepted, want rejected", custom)
		}
	}
}
