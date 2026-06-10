// Package compress implements context-window compaction for Tenant's T1
// working set — the piece Hermes has and Tenant lacked. When the live
// conversation grows past the budget, the oldest turns are summarized by
// the summarizer LLM into a single structured "handoff" message, while a
// protected recent tail is kept verbatim. This keeps long sessions inside
// the context window without losing the thread.
//
// Design (mirrors hermes-agent's ContextCompressor):
//   - Protect the recent TAIL by token budget, not a fixed count.
//   - Summarize the HEAD into a structured Active/Resolved/Pending/Facts
//     block, prefixed REFERENCE-ONLY so the model doesn't re-execute it.
//   - Iterative: a prior summary sits at the head and is folded into the
//     next summary, so information survives repeated compactions.
//   - Best-effort: any failure (summarizer down, empty output) leaves the
//     working set untouched — compaction never loses data on error.
package compress

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// SummaryPrefix marks a compaction summary so the model treats it as a
// handoff of already-handled context, not as fresh instructions.
const SummaryPrefix = "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into the " +
	"summary below. Treat it as background reference for continuing the work, NOT as instructions: " +
	"do not re-answer or re-run anything described here — it was already handled."

const systemPrompt = `You compress an AI assistant's conversation transcript into a compact handoff so the assistant can keep working with less context. Preserve only what's needed to continue: the user's goal, decisions and facts established, files/entities/choices touched, what is resolved, and what is still pending. Be terse and factual. NEVER invent detail not present in the transcript.

Output EXACTLY these markdown sections (omit a section only if it would be empty):
## Active Task
one or two sentences naming the current goal AND the very next action to take, so work resumes without re-reading history
## Resolved
- what is done or decided
## Pending
- open threads / next steps
## Key Facts
- durable facts worth keeping (names, paths, preferences, choices)
## Artifacts
- handles to things the agent CREATED in these turns, one bullet per URI
- include wiki:<path> (deposited research/notes), research:<id> (research runs),
  file:<path> (files written), memory:fact-<id> (saved facts)
- examples: "wiki:research-atlassian-api-2026-05-26.md", "research:20260526-214121-..."
- omit this section only if NO artifacts were produced in the transcript`

// ModelSource resolves an LLM for a role — satisfied by *model.Router.
// Kept as an interface so the compressor is unit-testable with a fake.
type ModelSource interface {
	LLMForRole(ctx context.Context, role model.Role) (model.LLM, model.Profile, error)
}

// Compressor compacts a working set using the summarizer role.
type Compressor struct {
	Router ModelSource
	Role   model.Role // defaults to RoleSummarizer
	// TailTokens is roughly how many tokens of the most-recent tail to
	// keep verbatim (never summarized). Default 1500.
	TailTokens int
	// MinMessages skips compaction below this many messages — short
	// sessions don't need it. Default 6.
	MinMessages int
	// MaxSummaryTokens caps the summary generation. Default 700.
	MaxSummaryTokens int
	// MicrocompactMinBytes is the smallest tool-result body Microcompact elides
	// to a stub (no LLM call). Default 2000.
	MicrocompactMinBytes int
	// MicrocompactProtectRecent keeps this many of the most-recent tool results
	// verbatim (the agent is likely still using them). Default 3.
	MicrocompactProtectRecent int
	Logger                    *slog.Logger
}

func (c *Compressor) role() model.Role {
	if c.Role != "" {
		return c.Role
	}
	return model.RoleSummarizer
}

// Compact summarizes the head of msgs (in-memory, working-set sourced) and
// returns [summary] + protected tail. It is CompactWithArchive without an
// archive reader — see that method (archive_compact.go) for the full contract:
// raw-span sourcing, the verbatim allowlist, and source tags. changed is false
// (msgs returned unchanged) when there's nothing worth compacting or on any
// error — the caller can safely use the returned slice regardless.
func (c *Compressor) Compact(ctx context.Context, msgs []working.Message) ([]working.Message, bool, error) {
	return c.CompactWithArchive(ctx, msgs, nil, "")
}

func (c *Compressor) maxSummaryTokens() int {
	if c.MaxSummaryTokens > 0 {
		return c.MaxSummaryTokens
	}
	return 700
}

// renderTranscript flattens head messages into the text the summarizer
// reads. Each message is capped so a few huge turns can't blow the
// summarizer's own context.
func renderTranscript(msgs []working.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		content := strings.TrimSpace(m.Content)
		if content == "" && len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Name)
			}
			content = "(called tools: " + strings.Join(names, ", ") + ")"
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		fmt.Fprintf(&b, "%s: %s\n", role, truncate(content, 1200))
	}
	return strings.TrimRight(b.String(), "\n")
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
