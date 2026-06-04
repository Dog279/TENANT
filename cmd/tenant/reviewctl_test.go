package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
)

// fakeLLM is a minimal model.LLM for testing /review without a real
// backend. canned[role-keyword] is matched against the system prompt
// to decide which response to return; err short-circuits to an error.
type fakeLLM struct {
	canned map[string]string // substring-of-system → response text
	err    error
	calls  int
}

func (f *fakeLLM) Generate(_ context.Context, req model.GenerateRequest) (model.GenerateResponse, error) {
	f.calls++
	if f.err != nil {
		return model.GenerateResponse{}, f.err
	}
	// Find the system message — it carries the reviewer soul.
	var sys string
	for _, m := range req.Messages {
		if m.Role == "system" {
			sys = m.Content
			break
		}
	}
	for needle, body := range f.canned {
		if strings.Contains(sys, needle) {
			return model.GenerateResponse{Text: body}, nil
		}
	}
	return model.GenerateResponse{Text: "(no canned match)"}, nil
}

func (f *fakeLLM) GenerateStream(ctx context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
	resp, err := f.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan model.StreamChunk, 1)
	ch <- model.StreamChunk{Delta: resp.Text}
	close(ch)
	return ch, nil
}

func (f *fakeLLM) TokenCount(_ context.Context, msgs []model.Message) (int, error) {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) / 4
	}
	return total, nil
}

// stubReviewControl bypasses LLM resolution entirely by overriding
// runOne for tests. The real reviewControl.runOne resolves an agent +
// router; for unit-test scope we want to drive section-aggregation,
// truncation, file-append, and reviewer-selection logic without a real
// agent. We do this by calling renderReviewReport / selectReviewers
// directly and using a Review-shaped struct that holds an LLM map.
type stubReview struct {
	byRole map[string]string // role → canned body; missing → returns error
}

// drive runs the same aggregation/render/append flow as
// reviewControl.Review but pulls bodies from stub.byRole instead of an
// LLM. Keeps the file-handling + section-ordering paths under test
// without spinning up a real Router/Agent.
func (s *stubReview) drive(planPath string, roles []string) (string, error) {
	raw, err := os.ReadFile(planPath)
	if err != nil {
		return "", err
	}
	plan := string(raw)
	if strings.TrimSpace(plan) == "" {
		return "", errors.New("review: plan file is empty")
	}
	truncated := false
	if len(plan) > reviewMaxPlanBytes {
		plan = plan[:reviewMaxPlanBytes]
		truncated = true
	}
	specs, err := selectReviewers(roles)
	if err != nil {
		return "", err
	}
	sections := make([]reviewSection, len(specs))
	for i, spec := range specs {
		body, ok := s.byRole[spec.Role]
		if !ok {
			sections[i] = reviewSection{Title: spec.Title, Err: errors.New("reviewer offline")}
			continue
		}
		sections[i] = reviewSection{Title: spec.Title, Body: body}
	}
	report := renderReviewReport(sections, truncated, time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC))
	if err := appendToFile(planPath, "\n\n"+report+"\n"); err != nil {
		return report, err
	}
	return report, nil
}

func writePlan(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readBack(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSelectReviewers — canonical-order preservation, alias rejection,
// empty input = all, whitespace stripping. These guards matter because
// /review's output sections must stay in CEO→Eng→Design order even when
// the operator asks for them in a different sequence.
func TestSelectReviewers(t *testing.T) {
	t.Run("empty returns all in catalog order", func(t *testing.T) {
		got, err := selectReviewers(nil)
		if err != nil || len(got) != 3 {
			t.Fatalf("want all 3, got %d err=%v", len(got), err)
		}
		if got[0].Role != "ceo" || got[1].Role != "eng" || got[2].Role != "design" {
			t.Errorf("wrong order: %v", got)
		}
	})
	t.Run("subset preserves catalog order", func(t *testing.T) {
		// Operator asked design,ceo — output must still be ceo first.
		got, err := selectReviewers([]string{"design", "ceo"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Role != "ceo" || got[1].Role != "design" {
			t.Errorf("subset order wrong: %v", got)
		}
	})
	t.Run("unknown role errors with hint", func(t *testing.T) {
		_, err := selectReviewers([]string{"ceo", "qa"})
		if err == nil || !strings.Contains(err.Error(), "qa") {
			t.Errorf("expected error mentioning qa, got %v", err)
		}
		if !strings.Contains(err.Error(), "known: ceo, eng, design") {
			t.Errorf("expected hint text in error, got %v", err)
		}
	})
	t.Run("whitespace + case", func(t *testing.T) {
		got, err := selectReviewers([]string{"  CEO ", "Eng"})
		if err != nil || len(got) != 2 {
			t.Fatalf("trim/case broken: %v err=%v", got, err)
		}
	})
}

// TestRenderReviewReport — header presence, section order, error
// rendering, truncation banner. Drift guards because the report is
// what the operator actually reads.
func TestRenderReviewReport(t *testing.T) {
	at := time.Date(2026, 5, 25, 14, 30, 0, 0, time.UTC)
	t.Run("all sections render in order", func(t *testing.T) {
		sections := []reviewSection{
			{Title: "CEO Review", Body: "MODE: HOLD SCOPE\n..."},
			{Title: "Engineering Review", Body: "VERDICT: pass\n..."},
			{Title: "Design Review", Body: "SCORES:\n- Hierarchy: 8/10..."},
		}
		got := renderReviewReport(sections, false, at)
		if !strings.HasPrefix(got, "## GSTACK REVIEW REPORT\n") {
			t.Errorf("missing header: %q", got[:60])
		}
		ceoIdx := strings.Index(got, "### CEO Review")
		engIdx := strings.Index(got, "### Engineering Review")
		desIdx := strings.Index(got, "### Design Review")
		if !(ceoIdx > 0 && ceoIdx < engIdx && engIdx < desIdx) {
			t.Errorf("section order wrong: ceo=%d eng=%d design=%d", ceoIdx, engIdx, desIdx)
		}
		if !strings.Contains(got, "Generated 2026-05-25 14:30 UTC") {
			t.Errorf("timestamp missing/wrong: %q", got)
		}
		if strings.Contains(got, "Plan truncated") {
			t.Errorf("non-truncated report should not show truncation banner")
		}
	})
	t.Run("truncation banner appears", func(t *testing.T) {
		got := renderReviewReport([]reviewSection{{Title: "X", Body: "y"}}, true, at)
		if !strings.Contains(got, "Plan truncated") {
			t.Errorf("truncated report missing banner: %q", got)
		}
	})
	t.Run("error section uses ERROR: prefix, others still render", func(t *testing.T) {
		got := renderReviewReport([]reviewSection{
			{Title: "CEO Review", Body: "ok"},
			{Title: "Engineering Review", Err: errors.New("network down")},
			{Title: "Design Review", Body: "scores"},
		}, false, at)
		if !strings.Contains(got, "ERROR: network down") {
			t.Errorf("error not surfaced: %q", got)
		}
		// Other sections must still render around the error one.
		if !strings.Contains(got, "ok") || !strings.Contains(got, "scores") {
			t.Errorf("non-error sections dropped: %q", got)
		}
	})
}

// TestAppendToFile_PreservesExistingContent — review must NEVER
// overwrite the plan. The existing plan content must be byte-equal in
// the prefix after we append.
func TestAppendToFile_PreservesExistingContent(t *testing.T) {
	original := "# My plan\n\nDo the thing.\n"
	path := writePlan(t, original)
	if err := appendToFile(path, "\n\nappended"); err != nil {
		t.Fatal(err)
	}
	got := readBack(t, path)
	if !strings.HasPrefix(got, original) {
		t.Errorf("original content corrupted; got %q", got)
	}
	if !strings.HasSuffix(got, "appended") {
		t.Errorf("append text missing; got %q", got)
	}
}

// TestStubReview_FullCascade_AppendsAllSections — happy path: 3
// reviewers, all return bodies, file gets the report appended in
// canonical order. Validates the end-to-end shape via the stub (LLM
// boundary is unit-tested separately via TestFakeLLM_SystemRouting).
func TestStubReview_FullCascade_AppendsAllSections(t *testing.T) {
	plan := "# Plan\n\nBuild X.\n"
	path := writePlan(t, plan)
	stub := &stubReview{byRole: map[string]string{
		"ceo":    "MODE: HOLD SCOPE\nVERDICT: solid.",
		"eng":    "VERDICT: pass\nTOP CONCERNS: none.",
		"design": "SCORES:\n- Hierarchy: 8/10.",
	}}
	report, err := stub.drive(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "MODE: HOLD SCOPE") || !strings.Contains(report, "VERDICT: pass") || !strings.Contains(report, "Hierarchy: 8/10") {
		t.Errorf("report missing reviewer bodies: %q", report)
	}
	file := readBack(t, path)
	if !strings.HasPrefix(file, plan) {
		t.Errorf("original plan corrupted")
	}
	if !strings.Contains(file, "## GSTACK REVIEW REPORT") {
		t.Errorf("report not appended to file: %q", file)
	}
}

// TestStubReview_OneReviewerFailsDoesNotAbortOthers — a reviewer
// without a canned body in the stub returns "reviewer offline"; the
// other two still render. The report includes the ERROR: section.
func TestStubReview_OneReviewerFailsDoesNotAbortOthers(t *testing.T) {
	path := writePlan(t, "# Plan\nstuff\n")
	stub := &stubReview{byRole: map[string]string{
		"ceo": "CEO says ship.",
		// eng: missing on purpose → triggers stub's error branch
		"design": "Design says ship.",
	}}
	report, err := stub.drive(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "ERROR: reviewer offline") {
		t.Errorf("eng section should have ERROR; got: %q", report)
	}
	if !strings.Contains(report, "CEO says ship.") || !strings.Contains(report, "Design says ship.") {
		t.Errorf("other reviewers dropped: %q", report)
	}
}

// TestStubReview_RejectsEmptyPlan — empty / whitespace-only plan must
// error before any LLM call. Avoids paying for reviews of nothing.
func TestStubReview_RejectsEmptyPlan(t *testing.T) {
	for _, body := range []string{"", "   \n\n  "} {
		path := writePlan(t, body)
		stub := &stubReview{byRole: map[string]string{"ceo": "x", "eng": "y", "design": "z"}}
		_, err := stub.drive(path, nil)
		if err == nil {
			t.Errorf("empty plan should error, body=%q", body)
		}
	}
}

// TestStubReview_RejectsMissingPlan — non-existent file must error,
// not produce a phantom report.
func TestStubReview_RejectsMissingPlan(t *testing.T) {
	stub := &stubReview{byRole: map[string]string{"ceo": "x"}}
	_, err := stub.drive(filepath.Join(t.TempDir(), "nope.md"), nil)
	if err == nil {
		t.Error("missing plan must error")
	}
}

// TestStubReview_SubsetOnlyRunsRequested — when roles is set, only
// those sections appear. The unrequested reviewer's body must NOT
// be in the output.
func TestStubReview_SubsetOnlyRunsRequested(t *testing.T) {
	path := writePlan(t, "# Plan\nstuff\n")
	stub := &stubReview{byRole: map[string]string{
		"ceo":    "CEO body",
		"eng":    "ENG body",
		"design": "DESIGN body",
	}}
	report, err := stub.drive(path, []string{"ceo", "design"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(report, "ENG body") {
		t.Errorf("eng was not requested but appears in report: %q", report)
	}
	if !strings.Contains(report, "CEO body") || !strings.Contains(report, "DESIGN body") {
		t.Errorf("requested reviewers missing from report: %q", report)
	}
}

// TestStubReview_Truncation — plan larger than reviewMaxPlanBytes is
// truncated and the report shows the truncation banner so the reader
// knows the review didn't see the full file.
func TestStubReview_Truncation(t *testing.T) {
	big := strings.Repeat("x", reviewMaxPlanBytes+1024)
	path := writePlan(t, big)
	stub := &stubReview{byRole: map[string]string{"ceo": "ok", "eng": "ok", "design": "ok"}}
	report, err := stub.drive(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "Plan truncated") {
		t.Errorf("truncation banner missing: %q", report[:200])
	}
}

// TestFakeLLM_SystemRouting — sanity check that the test fake routes by
// system-prompt substring as designed. Underpins the assumption that we
// could (later) test the real Generate call path by injecting fakeLLM.
func TestFakeLLM_SystemRouting(t *testing.T) {
	llm := &fakeLLM{canned: map[string]string{
		"CEO":          "ceo verdict",
		"Engineering":  "eng verdict",
		"product desi": "design verdict", // matches "senior product designer"
	}}
	cases := []struct {
		sys, want string
	}{
		{"You are a startup CEO reviewer.", "ceo verdict"},
		{"Engineering manager reviewing the plan.", "eng verdict"},
		{"You are a senior product designer.", "design verdict"},
	}
	for _, c := range cases {
		resp, err := llm.Generate(context.Background(), model.GenerateRequest{
			Messages: []model.Message{{Role: "system", Content: c.sys}, {Role: "user", Content: "x"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Text != c.want {
			t.Errorf("fake routing for %q: got %q want %q", c.sys, resp.Text, c.want)
		}
	}
}

// TestReviewerSoulsEmbedded — drift guard: each built-in soul must be
// embedded non-empty AND must mention its role-specific terminology, so
// a slipped rename of the .md file or accidental empty file fails CI.
func TestReviewerSoulsEmbedded(t *testing.T) {
	cases := []struct {
		name, body, needle string
	}{
		{"ceo", reviewerCEOSoul, "SCOPE EXPANSION"},
		{"eng", reviewerEngSoul, "Architecture"},
		{"design", reviewerDesignSoul, "Hierarchy"},
	}
	for _, c := range cases {
		if strings.TrimSpace(c.body) == "" {
			t.Errorf("%s soul is empty — embed broken?", c.name)
		}
		if !strings.Contains(c.body, c.needle) {
			t.Errorf("%s soul missing expected marker %q (drift guard)", c.name, c.needle)
		}
	}
}
