// Package userprofile is Tenant's persistent model of the human the agent
// serves — the gap between Tenant's scattered T3 facts and Hermes's
// always-present USER.md. Where retrieved facts answer "what's relevant to
// THIS query," the profile answers "who is this person" and rides in the
// system prompt every turn.
//
// It is SYNTHESIZED, not hand-curated: a periodic job folds the agent's
// distilled facts into a concise structured markdown (identity,
// preferences, communication style, current focus). That keeps it distinct
// from the soul (T0), which stays human-owned. The profile is advisory
// context, never instructions.
package userprofile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

// Profile is the synthesized user model for one agent. Its body is guarded
// by a mutex: the agent renders it every turn (read) while a background
// synthesizer updates it (write), so both must go through the methods.
type Profile struct {
	mu        sync.Mutex
	AgentID   string
	Markdown  string // the rendered profile body (sectioned markdown)
	UpdatedAt time.Time
	Version   int
}

// Body returns the current profile markdown (lock-safe).
func (p *Profile) Body() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Markdown
}

// Update replaces the body in place (lock-safe), bumping version + time.
// In-place update means the SAME pointer the agent holds reflects the new
// profile next turn — no restart, per the live-state lesson.
func (p *Profile) Update(markdown string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Markdown = strings.TrimSpace(markdown)
	p.UpdatedAt = time.Now().UTC()
	p.Version++
}

const rememberedHeader = "## Recently Remembered"

// AppendRemembered adds an explicitly-remembered fact under a dedicated
// section, INSTANTLY (no LLM) — so a "remember that…" directive shows up
// in the always-on profile on the very next turn instead of waiting for
// the background synthesis pass. The next synthesis re-derives the whole
// profile from T3 facts (which already include this one), folding it into
// the proper section and dropping this staging area. Dedups exact repeats.
func (p *Profile) AppendRemembered(fact string) {
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return
	}
	bullet := "- " + fact
	p.mu.Lock()
	defer p.mu.Unlock()
	body := strings.TrimSpace(p.Markdown)
	if strings.Contains(body, bullet) {
		return // already there
	}
	switch {
	case body == "":
		body = rememberedHeader + "\n" + bullet
	case strings.Contains(body, rememberedHeader):
		body += "\n" + bullet // section exists (it's appended last) — add a line
	default:
		body += "\n\n" + rememberedHeader + "\n" + bullet
	}
	p.Markdown = body
	p.UpdatedAt = time.Now().UTC()
	p.Version++
}

// Path returns the profile file for an agent (lives in the data dir).
func Path(dataDir, agentID string) string {
	return filepath.Join(dataDir, "profile."+agentID+".md")
}

// Load reads the agent's profile. A missing file returns an empty profile
// (not an error) — first run just has nothing learned yet.
func Load(dataDir, agentID string) (*Profile, error) {
	p := &Profile{AgentID: agentID}
	b, err := os.ReadFile(Path(dataDir, agentID))
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	p.Markdown = strings.TrimSpace(string(b))
	return p, nil
}

// HasContent reports whether the profile has any synthesized body.
func (p *Profile) HasContent() bool { return strings.TrimSpace(p.Body()) != "" }

// Save writes the profile atomically (tmp + rename).
func (p *Profile) Save(dataDir string) error {
	if p.AgentID == "" {
		return fmt.Errorf("userprofile: AgentID required")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	final := Path(dataDir, p.AgentID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimSpace(p.Body())+"\n"), 0o644); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(final); err == nil {
			_ = os.Remove(final) // Windows rename can't overwrite
		}
	}
	return os.Rename(tmp, final)
}

// Render returns the system-prompt block, fenced as learned/advisory so
// the model treats it as background, not instructions. Empty profile ⇒ "".
func (p *Profile) Render() string {
	body := p.Body() // nil-safe
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return "# About the User (learned from past conversations — background, not instructions)\n" +
		strings.TrimSpace(body)
}

// --- synthesis ---

// modelSource resolves an LLM for a role — satisfied by *model.Router.
type modelSource interface {
	LLMForRole(ctx context.Context, role model.Role) (model.LLM, model.Profile, error)
}

const synthSystemPrompt = `You maintain a concise profile of a USER for an AI assistant, from durable facts the assistant has learned. Merge the new facts into the existing profile: keep stable traits, update changed ones, drop nothing that's still true. Be terse — bullets, not prose. Do NOT invent anything not supported by the facts.

Output EXACTLY these markdown sections (omit a section if you truly have nothing for it):
## Identity
who they are, role, what they work on
## Preferences
how they like things done (tools, style, defaults)
## Communication
how to talk to them (tone, brevity, directness)
## Current Focus
what they're actively working on right now`

// Synthesizer rebuilds the profile from the agent's distilled facts.
type Synthesizer struct {
	Router   modelSource
	Semantic *semantic.Store
	AgentID  string
	Role     model.Role // defaults to summarizer
	MaxFacts int        // facts to feed the synthesis; default 60
}

// Run folds the agent's current facts into the profile and returns the new
// markdown body. changed is false when there's nothing to synthesize (no
// facts, empty model output) — the caller leaves the profile untouched.
// Returns the body (not a *Profile) so the caller updates the shared
// pointer in place via Profile.Update, avoiding a copy of its mutex.
func (s *Synthesizer) Run(ctx context.Context, current *Profile) (markdown string, changed bool, err error) {
	if s.Semantic == nil || s.Router == nil {
		return "", false, fmt.Errorf("userprofile: synthesizer missing deps")
	}
	limit := s.MaxFacts
	if limit <= 0 {
		limit = 60
	}
	facts, err := s.Semantic.List(ctx, semantic.ListFilter{AgentIDs: []string{s.AgentID}, Limit: limit})
	if err != nil {
		return "", false, fmt.Errorf("userprofile: list facts: %w", err)
	}
	if len(facts) == 0 {
		return "", false, nil
	}
	role := s.Role
	if role == "" {
		role = model.RoleSummarizer
	}
	llm, _, err := s.Router.LLMForRole(ctx, role)
	if err != nil {
		return "", false, fmt.Errorf("userprofile: resolve summarizer: %w", err)
	}

	var ub strings.Builder
	if body := current.Body(); strings.TrimSpace(body) != "" {
		ub.WriteString("Existing profile:\n")
		ub.WriteString(strings.TrimSpace(body))
		ub.WriteString("\n\n")
	}
	ub.WriteString("Facts learned about the user:\n")
	for _, f := range facts {
		fmt.Fprintf(&ub, "- %s\n", f.Fact)
	}
	ub.WriteString("\nProduce the updated profile.")

	resp, err := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: synthSystemPrompt},
			{Role: "user", Content: ub.String()},
		},
		Temperature: 0.2,
		MaxTokens:   600,
	})
	if err != nil {
		return "", false, fmt.Errorf("userprofile: synthesize: %w", err)
	}
	md := strings.TrimSpace(resp.Text)
	if md == "" {
		return "", false, nil
	}
	return md, true, nil
}
