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
// agent can pull relevant T4 recipes into the prompt.
type skillRetriever struct {
	st      *skills.Store
	agentID string
}

func (r skillRetriever) RetrieveSkills(ctx context.Context, emb []float32, k int) ([]agent.SkillCard, error) {
	hits, err := r.st.Search(ctx, skills.Query{AgentID: r.agentID, Embedding: emb, K: k})
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
		Description: "Save a reusable procedure (a 'skill') so it can be recalled for similar future tasks. Use after you work out a multi-step approach worth keeping.",
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
	var embed []float32
	if t.emb != nil {
		if vecs, err := t.emb.Embed(ctx, []string{a.Name + ": " + a.Description}); err == nil && len(vecs) == 1 {
			embed = vecs[0]
		}
	}
	id, err := t.st.Upsert(ctx, &skills.Skill{
		AgentID: t.agentID, Name: a.Name, Description: a.Description, Recipe: a.Recipe,
		Status: skills.StatusLive, Enabled: true, Embedding: embed,
	})
	if err != nil {
		return "skill save failed: " + err.Error(), true, nil
	}
	return fmt.Sprintf("saved skill %q (#%d)", a.Name, id), false, nil
}

// skillControl implements tui.SkillControl for the /skills command.
type skillControl struct {
	st      *skills.Store
	emb     model.Embedder
	agentID string
}

func (c skillControl) SkillList() []tui.SkillInfo {
	list, err := c.st.List(context.Background(), skills.ListFilter{AgentID: c.agentID, IncludeDisabled: true})
	if err != nil {
		return nil
	}
	out := make([]tui.SkillInfo, 0, len(list))
	for _, sk := range list {
		out = append(out, tui.SkillInfo{Name: sk.Name, Description: sk.Description, Enabled: sk.Enabled, Status: sk.Status})
	}
	return out
}

func (c skillControl) AddSkill(name, description, recipe string) error {
	var embed []float32
	if c.emb != nil {
		if vecs, err := c.emb.Embed(context.Background(), []string{name + ": " + description}); err == nil && len(vecs) == 1 {
			embed = vecs[0]
		}
	}
	_, err := c.st.Upsert(context.Background(), &skills.Skill{
		AgentID: c.agentID, Name: name, Description: description, Recipe: recipe,
		Status: skills.StatusLive, Enabled: true, Embedding: embed,
	})
	return err
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
