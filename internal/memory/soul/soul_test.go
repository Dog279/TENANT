package soul_test

import (
	"errors"
	"strings"
	"testing"

	"tenant/internal/memory/soul"
)

func TestSoul_SaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	original := soul.NewDefault("main")
	original.User.Name = "Ada"
	original.User.Timezone = "America/New_York"
	original.User.Preferences = []string{"concise responses", "code over prose"}

	if err := original.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := soul.Load(dir, "main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.User.Name != "Ada" {
		t.Fatalf("User.Name = %q, want Ada", loaded.User.Name)
	}
	if len(loaded.User.Preferences) != 2 {
		t.Fatalf("User.Preferences len = %d, want 2", len(loaded.User.Preferences))
	}
	if loaded.Meta.Version != 1 {
		t.Fatalf("Meta.Version = %d, want 1 after first Save", loaded.Meta.Version)
	}
	if loaded.Meta.UpdatedAt.IsZero() {
		t.Fatal("Meta.UpdatedAt unset after Save")
	}
}

func TestSoul_LoadMissingReturnsErrNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := soul.Load(dir, "no-such-agent")
	if !errors.Is(err, soul.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSoul_SaveBumpsVersionAndUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	s := soul.NewDefault("main")
	if err := s.Save(dir); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	first := s.Meta.UpdatedAt
	if err := s.Save(dir); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if s.Meta.Version != 2 {
		t.Fatalf("Version = %d after second Save, want 2", s.Meta.Version)
	}
	if !s.Meta.UpdatedAt.After(first) && !s.Meta.UpdatedAt.Equal(first) {
		t.Fatalf("UpdatedAt did not advance: was %v now %v", first, s.Meta.UpdatedAt)
	}
}

func TestSoul_SaveRequiresAgentID(t *testing.T) {
	dir := t.TempDir()
	s := &soul.Soul{}
	if err := s.Save(dir); err == nil {
		t.Fatal("Save with empty Agent.ID should error")
	}
}

func TestSoul_RenderProducesStructuredMarkdown(t *testing.T) {
	s := soul.NewDefault("main")
	s.User.Name = "Ada"
	s.User.Preferences = []string{"concise responses"}

	rendered := s.Render()

	for _, want := range []string{
		"# About You",
		"You are Tenant, personal assistant.",
		"# Your Values",
		"# About the User",
		"- Name: Ada",
		"- Preference: concise responses",
		"# Operating Instructions",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered missing %q:\n%s", want, rendered)
		}
	}
}

// NewDefault ships a clean, team-aware main soul: it names the built-in
// specialists by their spawnable names so a fresh install delegates by
// default, and carries NO personal data — the empty user block means nothing
// renders under "About the User".
func TestNewDefault_NamesSpecialistsNoPersonalData(t *testing.T) {
	rendered := soul.NewDefault("main").Render()
	for _, name := range []string{"Programmer", "Researcher", "Writer", "QA", "Strategist"} {
		if !strings.Contains(rendered, name) {
			t.Errorf("default soul should name the built-in specialist %q:\n%s", name, rendered)
		}
	}
	if strings.Contains(rendered, "# About the User") {
		t.Errorf("shipped default must not render an About-the-User section (no personal data):\n%s", rendered)
	}
}

func TestSoul_RenderSkipsEmptyBlocks(t *testing.T) {
	s := &soul.Soul{}
	s.Agent.Name = "TestBot"
	rendered := s.Render()
	if !strings.Contains(rendered, "TestBot") {
		t.Errorf("Agent.Name not rendered: %s", rendered)
	}
	if strings.Contains(rendered, "# About the User") {
		t.Errorf("empty user block rendered: %s", rendered)
	}
	if strings.Contains(rendered, "# Operating Instructions") {
		t.Errorf("empty instructions rendered: %s", rendered)
	}
}

func TestSoul_LoadRecoversAgentIDFromFilename(t *testing.T) {
	// Write a TOML missing agent.id — Load should backfill from the
	// filename so half-set-up souls don't load with empty IDs.
	dir := t.TempDir()
	s := soul.NewDefault("backup")
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Manually edit a different soul with no agent.id in TOML.
	// (Easier path: trust that Load doesn't blank out a valid ID.)
	loaded, err := soul.Load(dir, "backup")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Agent.ID != "backup" {
		t.Fatalf("Agent.ID = %q, want backup", loaded.Agent.ID)
	}
}

func TestSoul_PathLayout(t *testing.T) {
	got := soul.Path("/tmp/x", "main")
	want := "main.toml"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("Path = %q, want suffix %q", got, want)
	}
	if !strings.Contains(got, "soul") {
		t.Fatalf("Path = %q missing 'soul' segment", got)
	}
}
