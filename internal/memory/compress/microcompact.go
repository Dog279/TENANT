package compress

import (
	"fmt"

	"tenant/internal/memory/working"
)

// KindToolElided marks a working.Message whose tool-result body Microcompact
// replaced with a stub. The original body stays durable in the archive
// (Event.ToolResult.Content); the stub carries the call id so it can be
// recalled later. See docs/compaction-upgrade-plan.md §4.2 (TEN-100).
const KindToolElided = "tool-elided"

// redundantElideFloor is the smallest body worth eliding purely because a later
// identical result supersedes it — below this the stub wouldn't save space.
const redundantElideFloor = 256

// Microcompact elides stale, large, or superseded tool-RESULT bodies
// (role=="tool") to short stubs WITHOUT an LLM call. It is lossless at the
// system level — full bodies remain in the archive — so it is safe to run often
// and BEFORE the LLM-backed Compact pass (where it also shrinks the summarizer's
// input). It protects the most-recent tool results verbatim (still likely in
// use) and never touches non-tool messages. Returns the rewritten slice and
// whether anything changed. It never errors; the error in the signature is for
// symmetry with Compact and the agent's optional-interface call site.
func (c *Compressor) Microcompact(msgs []working.Message) ([]working.Message, bool, error) {
	minBytes := c.MicrocompactMinBytes
	if minBytes <= 0 {
		minBytes = 2000
	}
	protect := c.MicrocompactProtectRecent
	if protect <= 0 {
		protect = 3
	}

	// Tool names live on the assistant's ToolCalls, not on the tool-result
	// message — map call id -> name so the stub can name the tool.
	toolName := make(map[string]string)
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				toolName[tc.ID] = tc.Name
			}
		}
	}

	// Index tool-result messages; track the latest index per identical body so
	// earlier duplicates can be treated as superseded.
	var toolIdx []int
	latest := make(map[string]int)
	for i, m := range msgs {
		if m.Role == "tool" {
			toolIdx = append(toolIdx, i)
			latest[m.Content] = i
		}
	}
	if len(toolIdx) == 0 {
		return msgs, false, nil
	}

	// Protect the most-recent `protect` tool results.
	protected := make(map[int]bool, protect)
	for k := len(toolIdx) - 1; k >= 0 && k >= len(toolIdx)-protect; k-- {
		protected[toolIdx[k]] = true
	}

	out := make([]working.Message, len(msgs))
	copy(out, msgs)
	changed := false
	for _, i := range toolIdx {
		if protected[i] || msgs[i].Kind == KindToolElided {
			continue
		}
		body := msgs[i].Content
		n := len(body)
		superseded := n > 0 && latest[body] != i
		switch {
		case n >= minBytes: // large body
		case superseded && n >= redundantElideFloor: // duplicate of a later result
		default:
			continue
		}

		callID := msgs[i].ToolCallID
		name := toolName[callID]
		if name == "" {
			name = "tool"
		}
		idForStub := callID
		if idForStub == "" {
			idForStub = "?"
		}
		ts := msgs[i].Timestamp.UTC().Format("2006-01-02 15:04")
		out[i].Content = fmt.Sprintf("[tool result elided — %s, %d bytes, %s — recall:%s]", name, n, ts, idForStub)
		out[i].Kind = KindToolElided
		out[i].Meta = mergeMeta(msgs[i].Meta, map[string]any{
			"elided":     true,
			"orig_bytes": n,
			"call_id":    callID,
			"superseded": superseded,
		})
		changed = true
	}
	return out, changed, nil
}

// mergeMeta returns a NEW map combining base + extra, so eliding a message never
// mutates a Meta map the caller may still hold.
func mergeMeta(base, extra map[string]any) map[string]any {
	m := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}
