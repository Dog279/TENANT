package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/agent"
	"tenant/internal/memory/skills"
	"tenant/internal/model"
	"tenant/internal/tui"
)

// skillRetriever adapts the skills.Store to agent.SkillRetriever so the
// agent can pull relevant T4 recipes into the prompt. Only eligible skills
// come back: trusted ones, and with tools set, ones whose required tools are
// enabled in the agent's registry (TEN-298).
type skillRetriever struct {
	st      *skills.Store
	agentID string
	tools   toolLister // the agent's tool registry; nil skips the tool check
}

// toolLister is a tool registry that lists its tools with their state.
type toolLister interface{ ToolList() []tui.ToolInfo }

func (r skillRetriever) RetrieveSkills(ctx context.Context, emb []float32, k int) ([]agent.SkillCard, error) {
	q := skills.Query{AgentID: r.agentID, Embedding: emb, K: k}
	if r.tools != nil {
		enabled := map[string]bool{}
		for _, ti := range r.tools.ToolList() {
			if ti.Enabled {
				enabled[ti.Name] = true
			}
		}
		q.Available = func(tool string) bool { return skillToolAvailable(enabled, tool) }
	}
	hits, err := r.st.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	cards := make([]agent.SkillCard, 0, len(hits))
	for _, h := range hits {
		cards = append(cards, agent.SkillCard{Name: h.Skill.Name, Description: h.Skill.Description, Recipe: h.Skill.Recipe})
	}
	return cards, nil
}

// skillTool is the agent-facing way to author a T4 skill (it satisfies
// the `plugin` interface so it slots into the tool mux and shows in
// /tools). The agent calls skill_save when it works out a reusable
// procedure worth keeping.
//
// What the agent saves is untrusted until the operator trusts it (TEN-298).
// A skill is injected into every later turn's operating rules, and the agent
// may be writing what a web page, an email or a relayed message told it to:
// so a saved skill, or an agent edit of a trusted one, is not offered to any
// agent until the operator reviews it.
type skillTool struct {
	st      *skills.Store
	emb     model.Embedder
	agentID string
}

func (skillTool) Tools() []model.ToolSpec {
	params := json.RawMessage(`{"type":"object","properties":{` +
		`"name":{"type":"string"},"description":{"type":"string","description":"one line: when to use this"},` +
		`"recipe":{"type":"string","description":"the steps to follow"}},"required":["name","description","recipe"]}`)
	return []model.ToolSpec{{
		Name:        "skill_save",
		Description: "Save a reusable procedure (a 'skill') so it can be recalled for similar future tasks. Use after you work out a multi-step approach worth keeping. The operator reviews a saved skill before it is used.",
		Parameters:  params,
	}}
}

func (t skillTool) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	var a struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Recipe      string `json:"recipe"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Description) == "" {
		return "skill name and description are required", true, nil
	}
	id, err := t.st.UpsertWithSource(ctx, &skills.Skill{
		AgentID: t.agentID, Name: a.Name, Description: a.Description, Recipe: a.Recipe,
		Status: skills.StatusLive, Enabled: true, Embedding: embedSkill(ctx, t.emb, a.Name, a.Description),
		Trust: skills.TrustUntrusted, Origin: skills.OriginAgent,
	}, skills.OriginAgent)
	if err != nil {
		return "skill save failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("saved skill %q (#%d). It is used once the operator reviews and trusts it (/skills trust %s).", a.Name, id, a.Name), false, nil
}

// skillControl implements tui.SkillControl for the /skills command.
type skillControl struct {
	st      *skills.Store
	emb     model.Embedder
	agentID string
	cfgDir  string // for the auto-accept policy (TEN-152); "" disables the toggle
}

// AutoAcceptMode returns the induced-skill auto-accept policy (TEN-152).
func (c skillControl) AutoAcceptMode() string {
	if c.cfgDir == "" {
		return "off"
	}
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil || lc.Improve.AutoAccept == "" {
		return "off"
	}
	return lc.Improve.AutoAccept
}

// SetAutoAccept persists a new auto-accept policy (off|on|trusted).
func (c skillControl) SetAutoAccept(mode string) error {
	if c.cfgDir == "" {
		return fmt.Errorf("auto-accept isn't configurable in this session")
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !validAutoAccept(mode) {
		return fmt.Errorf("mode must be off, on, or trusted")
	}
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		return err
	}
	if mode == "off" {
		mode = "" // store empty = default-off
	}
	lc.Improve.AutoAccept = mode
	return lc.Save(c.cfgDir)
}

func (c skillControl) SkillList() []tui.SkillInfo {
	list, err := c.st.List(context.Background(), skills.ListFilter{AgentID: c.agentID, IncludeDisabled: true})
	if err != nil {
		return nil
	}
	out := make([]tui.SkillInfo, 0, len(list))
	for _, sk := range list {
		out = append(out, tui.SkillInfo{Name: sk.Name, Description: sk.Description, Enabled: sk.Enabled, Status: sk.Status,
			Trusted: sk.Trust == skills.TrustTrusted, Origin: sk.Origin})
	}
	return out
}

// AddSkill saves a skill the operator wrote: trusted, since they wrote it.
func (c skillControl) AddSkill(name, description, recipe string) error {
	return c.addSkill(name, description, recipe, skills.OriginOperator)
}

// addSkill saves a trusted skill with the given origin (operator or seed),
// which is also its history's change source.
func (c skillControl) addSkill(name, description, recipe, origin string) error {
	ctx := context.Background()
	_, err := c.st.UpsertWithSource(ctx, &skills.Skill{
		AgentID: c.agentID, Name: name, Description: description, Recipe: recipe,
		Status: skills.StatusLive, Enabled: true, Embedding: embedSkill(ctx, c.emb, name, description),
		Trust: skills.TrustTrusted, Origin: origin,
	}, origin)
	return err
}

// ImportSkill imports an Agent Skills package (TEN-298): untrusted unless
// trust is set.
func (c skillControl) ImportSkill(path string, trust bool) (string, error) {
	res, err := importSkillPackage(context.Background(), c.st, c.emb, c.agentID, path, trust)
	if err != nil {
		return "", err
	}
	return res.Summary(), nil
}

// ExportSkill writes a skill as dir/<name>/SKILL.md and returns the directory.
func (c skillControl) ExportSkill(name, dir string) (string, error) {
	return exportSkillPackage(context.Background(), c.st, c.agentID, name, dir)
}

// SetSkillTrust trusts or distrusts a skill; false when there is no such skill.
func (c skillControl) SetSkillTrust(name string, trusted bool) (bool, error) {
	trust := skills.TrustUntrusted
	if trusted {
		trust = skills.TrustTrusted
	}
	return c.st.SetTrust(context.Background(), c.agentID, name, trust)
}

// SkillDetail describes one skill for /skills show: its text, trust,
// provenance, required tools and whether it is offered to the agent, and why
// not. nil when there is no such skill.
func (c skillControl) SkillDetail(name string) (*tui.SkillDetail, error) {
	sk, err := c.st.GetByName(context.Background(), c.agentID, name)
	if err != nil || sk == nil {
		return nil, err
	}
	d := &tui.SkillDetail{
		Name: sk.Name, Description: sk.Description, Recipe: sk.Recipe, Status: sk.Status, Enabled: sk.Enabled,
		Trusted: sk.Trust == skills.TrustTrusted, Origin: sk.Origin, Version: sk.Version, RequiresTools: sk.RequiresTools,
	}
	if ok, why := sk.Eligible(nil); !ok {
		d.NotOffered = why
	}
	if p := sk.Package; p != nil {
		d.Source, d.Digest, d.License, d.Compatibility, d.Files = p.Source, p.Digest, p.License, p.Compatibility, p.Files
		d.ImportedAt = p.ImportedAt
	}
	return d, nil
}

func (c skillControl) SetSkillEnabled(name string, on bool) (bool, error) {
	return c.st.SetEnabledByName(context.Background(), c.agentID, name, on)
}

func (c skillControl) ForgetSkill(name string) (bool, error) {
	list, err := c.st.List(context.Background(), skills.ListFilter{AgentID: c.agentID, IncludeDisabled: true})
	if err != nil {
		return false, err
	}
	for _, sk := range list {
		if sk.Name == name {
			return true, c.st.Tombstone(context.Background(), sk.ID)
		}
	}
	return false, nil
}

func (c skillControl) AcceptSkill(name string) (bool, error) {
	list, err := c.st.List(context.Background(), skills.ListFilter{AgentID: c.agentID, Status: skills.StatusProposed, IncludeDisabled: true})
	if err != nil {
		return false, err
	}
	for _, sk := range list {
		if sk.Name == name {
			return true, c.st.Accept(context.Background(), sk.ID)
		}
	}
	return false, nil
}

func (c skillControl) SkillHistory(name string) ([]tui.SkillHistoryEntry, error) {
	hs, err := c.st.History(context.Background(), c.agentID, name)
	if err != nil {
		return nil, err
	}
	out := make([]tui.SkillHistoryEntry, 0, len(hs))
	for _, h := range hs {
		out = append(out, tui.SkillHistoryEntry{
			Version: h.Version, PriorDescription: h.PriorDescription,
			PriorRecipe: h.PriorRecipe, PriorStatus: h.PriorStatus,
			ChangeSource: h.ChangeSource, ChangedAt: h.ChangedAt,
		})
	}
	return out, nil
}

func (c skillControl) SkillCurrent(name string) (*tui.SkillSnapshot, error) {
	cur, err := c.st.GetByName(context.Background(), c.agentID, name)
	if err != nil || cur == nil {
		return nil, err
	}
	return &tui.SkillSnapshot{
		Name: cur.Name, Description: cur.Description,
		Recipe: cur.Recipe, Status: cur.Status,
	}, nil
}

func (c skillControl) SkillRevert(name string, version int) error {
	reverted, err := c.st.RevertTo(context.Background(), c.agentID, name, version)
	if err != nil {
		return err
	}
	// Re-embed against the restored description so retrieval picks the
	// restored content correctly. Best-effort: a degraded embedder is
	// not a reason to fail the revert.
	if c.emb != nil && reverted != nil {
		if vecs, err := c.emb.Embed(context.Background(), []string{reverted.Name + ": " + reverted.Description}); err == nil && len(vecs) == 1 {
			reverted.Embedding = vecs[0]
			_, _ = c.st.UpsertWithSource(context.Background(), reverted, "revert")
		}
	}
	return nil
}
