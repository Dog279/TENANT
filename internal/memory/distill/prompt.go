package distill

import (
	"fmt"
	"strings"

	"tenant/internal/memory/episodic"
)

// systemPrompt is the instruction the summarizer gets every batch.
// Kept tight: small local models do worse with long instructions.
const systemPrompt = `You extract durable facts from conversation snippets.

Rules:
- Output one-sentence atomic facts about the USER, their PROJECTS, or their PREFERENCES.
- Only LASTING properties. "user is debugging X right now" is bad; "user works on project X" is good.
- Skip greetings, small talk, and one-off questions.
- If a snippet contains no fact-worthy content, omit it from the output.
- Be specific: prefer "user prefers Go for backend" over "user likes programming".
- Each fact lists the source_episode_ids that supported it (from the bracketed numbers below).
- Confidence: 0.5-0.7 if mentioned once with no follow-up; 0.7-0.9 if explicit; 0.9-1.0 if user stated as a strong preference.
- Importance (1-10): how load-bearing the fact is for long-term work. 1 = transient/mundane (a passing mention, small talk). 5 = ordinary preference or detail. 8-10 = load-bearing project architecture, a hard constraint, a key decision and its rationale, an identity fact, or a deadline. When unsure, use 5.

Respond with JSON only matching this schema:
{"facts":[{"fact":"...","confidence":0.0-1.0,"importance":1-10,"source_episode_ids":[id,...]}]}

If nothing fact-worthy is present, respond with {"facts":[]}.`

// factsJSONSchema constrains the summarizer's output when the
// active model supports grammar-constrained generation.
const factsJSONSchema = `{
  "type": "object",
  "properties": {
    "facts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "fact": {"type": "string", "minLength": 1},
          "confidence": {"type": "number", "minimum": 0, "maximum": 1},
          "importance": {"type": "integer", "minimum": 1, "maximum": 10},
          "source_episode_ids": {"type": "array", "items": {"type": "integer"}}
        },
        "required": ["fact", "confidence", "source_episode_ids"]
      }
    }
  },
  "required": ["facts"]
}`

// buildPrompt renders a batch of episodes into the user message the
// summarizer reads. Format: numbered list with [id] markers so the
// model can cite source episodes by their actual IDs.
func buildPrompt(batch []*episodic.Episode) string {
	var b strings.Builder
	b.WriteString("Recent conversations:\n\n")
	for _, e := range batch {
		fmt.Fprintf(&b, "[%d] user: %s\n", e.ID, truncate(e.Prompt, 600))
		fmt.Fprintf(&b, "    assistant: %s\n\n", truncate(e.Response, 600))
	}
	b.WriteString("Extract durable facts from the above. Use the bracketed numbers in source_episode_ids.")
	return b.String()
}

// truncate caps a single message to n chars to keep prompts bounded.
// Distillation works on summaries; precise text isn't load-bearing.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
