package main

import (
	"context"
	"encoding/json"
	"strings"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

// memoryTool is the agent-facing way to WRITE durable memory on a user
// directive ("remember that…", "from now on…"). It satisfies the plugin
// interface so it slots into the tool mux and shows in /tools.
//
// It writes only to T3 semantic facts — the durable, retrievable store
// that also feeds the user-profile synthesis. It deliberately CANNOT touch
// the soul/identity (T0): that stays operator-curated so the agent's
// persona behaves predictably. The model persists facts; only the human
// edits who the agent is.
type memoryTool struct {
	sem        *semantic.Store
	emb        model.Embedder
	embedderID string
	agentID    string
	// note, if set, records the fact in the always-on user profile
	// immediately — so an explicit directive takes effect next turn, not
	// on the background synthesis cadence. Optional.
	note func(fact string)
}

func (memoryTool) Tools() []model.ToolSpec {
	params := json.RawMessage(`{"type":"object","properties":{` +
		`"fact":{"type":"string","description":"one durable, self-contained sentence to remember about the user, their preferences, or their work"}` +
		`},"required":["fact"]}`)
	return []model.ToolSpec{{
		Name: "memory_remember",
		Description: "Persist a durable fact to long-term memory when the user asks you to remember something, " +
			"or states a lasting preference or fact about themselves or their work. Write ONE atomic, self-contained " +
			"sentence (it must make sense without the surrounding conversation). Do NOT use this for transient or " +
			"conversational details. This writes to factual memory only — it cannot change your identity/persona.",
		Parameters: params,
	}}
}

func (t memoryTool) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	var a struct {
		Fact string `json:"fact"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	fact := strings.TrimSpace(a.Fact)
	if fact == "" {
		return "fact is required", true, nil
	}
	if t.sem == nil || t.emb == nil || t.embedderID == "" {
		return "memory is unavailable in this session (no embedder configured)", true, nil
	}
	vecs, err := t.emb.Embed(ctx, []string{fact})
	if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		// T3 requires an embedding to be retrievable; surface honestly
		// rather than silently dropping the user's directive.
		return "could not save to memory: the embedder is unavailable, so this fact can't be made retrievable. Try again once embeddings are back.", true, nil
	}
	id, err := t.sem.Insert(ctx, &semantic.Fact{
		AgentID:    t.agentID,
		Visibility: semantic.VisibilityPrivate,
		Fact:       fact,
		Confidence: 0.95, // explicit user directive ⇒ high confidence
		EmbedderID: t.embedderID,
		Embedding:  vecs[0],
	})
	if err != nil {
		return "memory write failed: " + err.Error(), true, nil
	}
	_ = id
	// Reflect it in the always-on profile right now so it's in context next
	// turn (the background pass would otherwise delay it minutes).
	if t.note != nil {
		t.note(fact)
	}
	return "remembered: " + fact, false, nil
}
