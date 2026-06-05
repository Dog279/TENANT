package main

// Session resume: on a COLD start of the interactive agent, seed the working set
// with a compact "bridge" recap of the operator's last session so they can say
// "continue" and the agent reconnects to what they were doing.
//
// Why this is safe (verified + debated, see tasks/session-resume-plan.md):
//   - Sourced from EPISODES (turn-pairs already in episodes.db). The bridge is
//     appended to the working set only; it is never inserted as an episode
//     (episodes are written from completed turns) and distillation reads
//     episodes, not the working set — so the recap can never recurse into
//     itself or pollute semantic facts.
//   - Wired at the single mainWorking site (cmdTUI) only, so sub-agents, the
//     Discord relay, eval, and one-shot CLI turns never resume. The gate is the
//     CALL SITE, never an AgentID check (AgentID "main" is shared by
//     tenant review/goal/os).
//   - Recent() is exact-agent + non-tombstoned, so no sub-agent leakage and no
//     operator-deleted turn resurfacing.
//   - Recency-gated: a stale session is not auto-resumed (misdirection > help).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tenant/internal/memory/assemble"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/working"
)

const (
	resumeEpisodes = 4               // last N turns to recap
	resumeMaxChars = 1500            // hard cap on the rendered bridge (budget hygiene)
	resumeMaxAge   = 48 * time.Hour  // don't auto-resume a session older than this
)

// seedResumeBridge appends a one-message session recap to w iff w is empty (cold
// start), recent episodes for exactly agentID exist, and the newest is within
// resumeMaxAge. Best-effort: any error or miss is a silent no-op — continuity is
// a nicety, never a turn-blocker. Returns the number of episodes recapped (0 =
// did not seed). now is injectable for tests.
func seedResumeBridge(ctx context.Context, w *working.Set, ep *episodic.Store, agentID string, now time.Time) int {
	if w == nil || ep == nil || w.Len() != 0 {
		return 0
	}
	eps, err := ep.Recent(ctx, agentID, resumeEpisodes)
	if err != nil || len(eps) == 0 {
		return 0
	}
	// Recency gate: eps is chronological, so the last is newest.
	if now.Sub(eps[len(eps)-1].Timestamp) > resumeMaxAge {
		return 0
	}
	bridge := renderSessionBridge(eps)
	if strings.TrimSpace(bridge) == "" {
		return 0
	}
	// Role=assistant (a prior-turn-shaped context message, not a higher-trust
	// mid-conversation system message); Kind="resume" so it can never be
	// mistaken for a compaction summary by the goal-header extractor.
	w.Append(working.Message{Role: "assistant", Kind: "resume", Content: bridge, Timestamp: now})
	return len(eps)
}

// renderSessionBridge formats recent episodes into a short, explicitly
// NON-directive recap (framed as background the agent may ignore), capped at
// resumeMaxChars. Artifact handles (wiki:/research:/file:/memory:) are preserved
// so the agent can re-grab what it produced last time.
func renderSessionBridge(eps []*episodic.Episode) string {
	if len(eps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Resuming — your last session\n")
	b.WriteString("(Recent activity, for continuity only. If the new request is about something else, ignore this and follow the new request.)\n")
	for _, e := range eps {
		date := e.Timestamp.Format("2006-01-02 15:04")
		fmt.Fprintf(&b, "- [%s] %s -> %s\n", date, resumeSnip(e.Prompt, 100), resumeSnip(e.Response, 180))
		if arts := assemble.ExtractArtifactURIs(e.Response); len(arts) > 0 {
			fmt.Fprintf(&b, "    artifacts: %s\n", strings.Join(arts, ", "))
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if r := []rune(out); len(r) > resumeMaxChars { // rune-safe cap
		out = string(r[:resumeMaxChars]) + "\n…(truncated)"
	}
	return out
}

func resumeSnip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
