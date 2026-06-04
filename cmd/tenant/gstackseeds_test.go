package main

import (
	"fmt"
	"strings"
	"testing"
)

// fakeSkillAdder satisfies the skillAdder interface installSkillSeeds takes.
// Captures AddSkill calls so we can verify the bundle installs each seed
// by-name without spinning up a real skills.Store.
type fakeSkillAdder struct {
	calls   []string
	failOn  string // name → fail on this one (test partial-failure path)
	failErr error
}

func (f *fakeSkillAdder) AddSkill(name, desc, recipe string) error {
	f.calls = append(f.calls, name)
	if name == f.failOn {
		return f.failErr
	}
	return nil
}

// installSkillSeeds installs every seed in the bundle. Each seed routes
// through AddSkill so the embedding + persistence path is identical to a
// manually-added skill.
func TestInstallSkillSeeds_GStack(t *testing.T) {
	f := &fakeSkillAdder{}
	n, err := installSkillSeeds("gstack", f)
	if err != nil {
		t.Fatalf("install gstack: %v", err)
	}
	if n != len(gstackSeeds) {
		t.Errorf("installed %d, want %d", n, len(gstackSeeds))
	}
	if len(f.calls) != len(gstackSeeds) {
		t.Fatalf("AddSkill called %d times, want %d", len(f.calls), len(gstackSeeds))
	}
	wantNames := map[string]bool{}
	for _, s := range gstackSeeds {
		wantNames[s.Name] = true
	}
	for _, called := range f.calls {
		if !wantNames[called] {
			t.Errorf("unexpected AddSkill: %q", called)
		}
		delete(wantNames, called)
	}
	if len(wantNames) > 0 {
		t.Errorf("not installed: %v", wantNames)
	}
}

// Unknown bundle → error, zero installed, zero AddSkill calls.
func TestInstallSkillSeeds_UnknownBundle(t *testing.T) {
	f := &fakeSkillAdder{}
	n, err := installSkillSeeds("madeup", f)
	if err == nil {
		t.Fatal("unknown bundle should error")
	}
	if n != 0 {
		t.Errorf("unknown bundle should install 0, got %d", n)
	}
	if len(f.calls) != 0 {
		t.Errorf("unknown bundle should call AddSkill 0 times, got %d", len(f.calls))
	}
	if !strings.Contains(err.Error(), "unknown bundle") {
		t.Errorf("error wording: %v", err)
	}
}

// Per-skill failure must NOT abort the bundle — operator gets a partial
// install + a clear error naming the failure. Idempotent retry will heal.
func TestInstallSkillSeeds_PartialFailureKeepsGoing(t *testing.T) {
	f := &fakeSkillAdder{
		failOn:  "founder-voice", // mid-catalog
		failErr: fmt.Errorf("embedder unavailable"),
	}
	n, err := installSkillSeeds("gstack", f)
	if err == nil {
		t.Fatal("expected first error to surface")
	}
	if !strings.Contains(err.Error(), "founder-voice") {
		t.Errorf("error should name the failed skill: %v", err)
	}
	if n != len(gstackSeeds)-1 {
		t.Errorf("installed %d, want %d (one failure)", n, len(gstackSeeds)-1)
	}
	if len(f.calls) != len(gstackSeeds) {
		t.Errorf("AddSkill calls = %d, want %d (every entry attempted)", len(f.calls), len(gstackSeeds))
	}
}

// Each catalog entry is well-formed: non-empty name + description + recipe,
// unique name, one-line description (drives retrieval embedding), recipe
// has substance (>100 chars).
func TestGStackSeeds_CatalogShape(t *testing.T) {
	if len(gstackSeeds) < 5 {
		t.Errorf("expected at least 5 gstack skills (design doc), got %d", len(gstackSeeds))
	}
	seen := map[string]bool{}
	for i, s := range gstackSeeds {
		if strings.TrimSpace(s.Name) == "" {
			t.Errorf("[%d] empty name", i)
		}
		if strings.TrimSpace(s.Description) == "" {
			t.Errorf("[%d] %q has empty description", i, s.Name)
		}
		if strings.TrimSpace(s.Recipe) == "" {
			t.Errorf("[%d] %q has empty recipe", i, s.Name)
		}
		if seen[s.Name] {
			t.Errorf("[%d] duplicate name %q", i, s.Name)
		}
		seen[s.Name] = true
		if strings.Contains(s.Description, "\n") {
			t.Errorf("%q: description has newlines (must be one line for embedding)", s.Name)
		}
		if len(s.Recipe) < 100 {
			t.Errorf("%q: recipe is suspiciously short (%d chars)", s.Name, len(s.Recipe))
		}
	}
	// Spot-check the 5 design-doc skills are present.
	for _, want := range []string{
		"investigate-systematically",
		"boil-the-lake-completeness",
		"structured-ask",
		"founder-voice",
		"status-escalation",
	} {
		if !seen[want] {
			t.Errorf("design-doc skill %q missing from catalog", want)
		}
	}
}

// founder-voice skill recipe MUST enforce the banned-word list — if someone
// refactors the recipe, this guards the discipline.
func TestGStackSeeds_FounderVoiceCarriesBannedWords(t *testing.T) {
	var voice *gstackSeedSkill
	for i := range gstackSeeds {
		if gstackSeeds[i].Name == "founder-voice" {
			voice = &gstackSeeds[i]
		}
	}
	if voice == nil {
		t.Fatal("founder-voice skill not in catalog")
	}
	for _, banned := range []string{
		"delve", "crucial", "robust", "comprehensive", "nuanced",
		"em-dashes", "here's the kicker",
	} {
		if !strings.Contains(voice.Recipe, banned) {
			t.Errorf("founder-voice recipe missing banned-word/phrase %q", banned)
		}
	}
}

// investigate-systematically MUST embed the Iron Law + 3-strike rule — same
// drift guard.
func TestGStackSeeds_InvestigateCarriesIronLaw(t *testing.T) {
	var inv *gstackSeedSkill
	for i := range gstackSeeds {
		if gstackSeeds[i].Name == "investigate-systematically" {
			inv = &gstackSeeds[i]
		}
	}
	if inv == nil {
		t.Fatal("investigate-systematically not in catalog")
	}
	for _, want := range []string{
		"NO FIXES WITHOUT ROOT CAUSE",
		"3-STRIKE",
		"INVESTIGATE",
		"ANALYZE",
		"HYPOTHESIZE",
		"IMPLEMENT",
	} {
		if !strings.Contains(inv.Recipe, want) {
			t.Errorf("investigate-systematically recipe missing %q", want)
		}
	}
}
