package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type fakeRules struct {
	rows    []ToolRuleRow
	sets    []string
	removes []string
	setErr  error
}

func (f *fakeRules) Rules() []ToolRuleRow { return f.rows }
func (f *fakeRules) SetRule(p, m string) error {
	f.sets = append(f.sets, p+"="+m)
	return f.setErr
}
func (f *fakeRules) RemoveRule(p string) (bool, error) {
	f.removes = append(f.removes, p)
	return p != "nothing", nil
}

func TestTools_RulesCard(t *testing.T) {
	s, _ := ssrServer()
	if body := get(t, s, "/tools").Body.String(); strings.Contains(body, "Tool rules") {
		t.Fatal("no rule control → no card")
	}
	fr := &fakeRules{rows: []ToolRuleRow{{Pattern: "delete_*", Mode: "deny", Kind: "glob"}, {Pattern: "write_note", Mode: "allow", Kind: "name"}}}
	s.SetRules(fr)
	body := get(t, s, "/tools").Body.String()
	for _, want := range []string{"Tool rules", "delete_*", "write_note", `action="/tools/rules/add"`, `action="/tools/rules/remove"`} {
		if !strings.Contains(body, want) {
			t.Errorf("tools page should contain %q", want)
		}
	}

	rec := ssrPost(t, s, "/tools/rules/add", url.Values{"pattern": {"mcp:127.0.0.1:9000:*"}, "mode": {"allow"}, "confirm": {"yes"}})
	if rec.Code != http.StatusSeeOther || len(fr.sets) != 1 || fr.sets[0] != "mcp:127.0.0.1:9000:*=allow" || !strings.Contains(rec.Header().Get("Location"), "msg=") {
		t.Errorf("add: code=%d sets=%v loc=%s", rec.Code, fr.sets, rec.Header().Get("Location"))
	}
	rec = ssrPost(t, s, "/tools/rules/add", url.Values{"pattern": {"x"}, "mode": {"maybe"}, "confirm": {"yes"}})
	if len(fr.sets) != 1 || !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Errorf("bad mode must be rejected before the control: sets=%v loc=%s", fr.sets, rec.Header().Get("Location"))
	}
	fr.setErr = errors.New("pattern must be one word")
	rec = ssrPost(t, s, "/tools/rules/add", url.Values{"pattern": {"bad"}, "mode": {"deny"}})
	// A plain message; the control's own words stay in the log (D21).
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=Couldn%27t+add+that+rule") || strings.Contains(loc, "one+word") {
		t.Errorf("control error should surface plainly: %s", loc)
	}

	rec = ssrPost(t, s, "/tools/rules/remove", url.Values{"pattern": {"delete_*"}, "mode": {"deny"}, "confirm": {"yes"}})
	if len(fr.removes) != 1 || fr.removes[0] != "delete_*" || !strings.Contains(rec.Header().Get("Location"), "msg=") {
		t.Errorf("remove: removes=%v loc=%s", fr.removes, rec.Header().Get("Location"))
	}
	rec = ssrPost(t, s, "/tools/rules/remove", url.Values{"pattern": {"nothing"}, "confirm": {"yes"}})
	if !strings.Contains(rec.Header().Get("Location"), "err=There%27s+no+rule") {
		t.Errorf("unknown rule: %s", rec.Header().Get("Location"))
	}
}

// REVSEC-1: whether removing a rule asks first is decided from the rule
// itself, not from a mode the request claims: a Block rule posted as
// mode=allow (in the body or the address) still meets the confirm step,
// and removing an allow rule, which only tightens, doesn't.
func TestRules_RemoveConfirmFollowsTheRule(t *testing.T) {
	s, _ := ssrServer()
	fr := &fakeRules{rows: []ToolRuleRow{{Pattern: "os_exec", Mode: "deny", Kind: "exact"}, {Pattern: "web_read", Mode: "allow", Kind: "exact"}}}
	s.SetRules(fr)
	for _, target := range []string{"/tools/rules/remove", "/tools/rules/remove?mode=allow"} {
		for _, form := range []url.Values{{"pattern": {"os_exec"}}, {"pattern": {"os_exec"}, "mode": {"allow"}}} {
			if rec := ssrPost(t, s, target, form); rec.Code != http.StatusPreconditionRequired || len(fr.removes) != 0 {
				t.Errorf("POST %s %v: %d, removes %v; want 428 and nothing removed", target, form, rec.Code, fr.removes)
			}
		}
	}
	if rec := ssrPost(t, s, "/tools/rules/remove", url.Values{"pattern": {"web_read"}, "mode": {"deny"}}); rec.Code != http.StatusSeeOther || len(fr.removes) != 1 {
		t.Errorf("removing an allow rule: %d, removes %v; want it done in one tap", rec.Code, fr.removes)
	}
	if rec := ssrPost(t, s, "/tools/rules/remove", url.Values{"pattern": {"os_exec"}, "confirm": {"yes"}}); rec.Code != http.StatusSeeOther || len(fr.removes) != 2 {
		t.Errorf("removing the Block rule, confirmed: %d, removes %v", rec.Code, fr.removes)
	}
}
