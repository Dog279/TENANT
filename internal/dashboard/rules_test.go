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
	rec = ssrPost(t, s, "/tools/rules/remove", url.Values{"pattern": {"nothing"}, "mode": {"allow"}})
	if !strings.Contains(rec.Header().Get("Location"), "err=There%27s+no+rule") {
		t.Errorf("unknown rule: %s", rec.Header().Get("Location"))
	}
}
