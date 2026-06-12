package dashboard

import (
	"strings"
	"testing"
)

// TEN-200: the home status board surfaces quality + skills when those controls
// are wired, links each card to its page, and degrades cleanly when they're nil.

func TestHome_StatusBoardWithQualityAndSkills(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetEval(&fakeEvalCtl{sched: EvalScheduleView{HasRun: true, LastScore: 89, Trend: "up"}})
	s.SetSkills(&fakeSkillCtl{skills: []SkillView{
		{Name: "a", Status: "live", Enabled: true},
		{Name: "b", Status: "proposed"},
	}})
	body := get(t, s, "/").Body.String()
	for _, want := range []string{"Quality", "89", "Skills learned", "waiting for your OK", `href="/eval"`, `href="/skills"`} {
		if !strings.Contains(body, want) {
			t.Errorf("home missing %q:\n%s", want, body)
		}
	}
}

func TestHome_GroupedNavPresent(t *testing.T) {
	s := New(Config{}, nil, nil, nil, nil, nil)
	body := get(t, s, "/").Body.String()
	for _, want := range []string{"nav-group", "Knowledge", `href="/skills"`, `href="/eval"`, "Capabilities"} {
		if !strings.Contains(body, want) {
			t.Errorf("grouped nav missing %q", want)
		}
	}
}

func TestHome_DegradesWithoutQualitySkills(t *testing.T) {
	// No eval/skills controls wired — home must still render, without those cards.
	s := New(Config{}, nil, nil, nil, nil, nil)
	body := get(t, s, "/").Body.String()
	if strings.Contains(body, "Skills learned") || strings.Contains(body, "Quality</div>") {
		t.Errorf("quality/skills cards should be hidden when controls are nil:\n%s", body)
	}
	if !strings.Contains(body, "healthy") {
		t.Error("home should still render the agent-healthy card")
	}
}
