package approval

// Per-tool permission rules: the operator's answer to tools the category table
// cannot know in advance: every tool a newly adopted MCP server brings, a
// plugin's unusual action id, a peer's tool. A rule maps a PATTERN to a mode
// and is consulted BEFORE the category logic on every broker (host, Discord
// relay, iMessage) and inside the connector gate, so it applies whichever
// surface a turn came from and whatever the connector's trust is:
//
//	allow  the tool runs without a prompt (even if its category is ask, or destructive)
//	deny   the tool is refused without a prompt (even under a trusted connector)
//	ask    the tool always prompts (even if its category is allow or a session grant exists)
//
// Patterns — the most specific match wins:
//
//	exact action id    mcp:127.0.0.1:9000:write_note      os_exec
//	bare tool name     write_note   → that tool on ANY connector, or the local tool of that name
//	glob               mcp:127.0.0.1:9000:*   delete_*   *:search_*   (* = any run, ? = one char)
//
// Rules persist in settings.<agent>.json under "tool_rules". Edit that file by
// hand and the running hub applies it within seconds (the settings watcher),
// or manage them live with /permissions add|rm and the dashboard's Tools page.
// There is deliberately NO agent tool for rules: the model must not be able to
// grant itself permissions. The file tools refuse the settings file outright,
// so write access does not open a way round that.

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"unicode"

	"tenant/internal/control"
	"tenant/internal/logs"
)

// Rules is a set of per-tool rules, safe for concurrent use.
type Rules struct {
	mu      sync.RWMutex
	rules   map[string]string // pattern → allow | deny | ask
	persist func(map[string]string)
	log     *slog.Logger
}

// NewRules returns an empty rule set.
func NewRules() *Rules {
	return &Rules{rules: map[string]string{}, log: logs.For("approvals")}
}

// DefaultRules is the process-wide rule set every broker and every connector
// consults. serve and the TUI load it from settings and set its persist hook.
var DefaultRules = NewRules()

// SetPersist installs the hook that saves the set after Set and Remove.
func (r *Rules) SetPersist(persist func(rules map[string]string)) {
	r.mu.Lock()
	r.persist = persist
	r.mu.Unlock()
}

// ruleKind classifies a pattern: exact id, bare tool name, or glob.
func ruleKind(pattern string) string {
	switch {
	case strings.ContainsAny(pattern, "*?"):
		return "glob"
	case strings.Contains(pattern, ":"):
		return "exact"
	}
	return "name"
}

// validRulePattern rejects patterns that cannot name a tool.
func validRulePattern(p string) error {
	if p == "" {
		return fmt.Errorf("pattern required (a tool name, an action id, or a glob like delete_*)")
	}
	if len(p) > 200 {
		return fmt.Errorf("pattern too long")
	}
	for _, r := range p {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return fmt.Errorf("pattern must be one word with no spaces")
		}
	}
	if strings.Trim(p, "*?") == "" {
		return fmt.Errorf("a pattern that matches everything is not allowed — use /permissions set <category> instead")
	}
	return nil
}

// globMatch matches s against pattern where '*' is any run of characters
// (including ':' and '/') and '?' is exactly one. Case-sensitive.
func globMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if pattern == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		default:
			if s == "" || s[0] != pattern[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		}
	}
	return s == ""
}

// matchRank reports whether pattern matches action and how specifically:
// 0 = exact id, 1 = bare name (the local tool or "<label>:<name>" on any
// connector), 2 = glob. Lower wins; ties break on longer literal patterns.
func matchRank(pattern, action string) (int, bool) {
	switch ruleKind(pattern) {
	case "glob":
		if globMatch(pattern, action) || (!strings.Contains(pattern, ":") && globMatch("*:"+pattern, action)) {
			return 2, true
		}
		return 0, false
	case "exact":
		if pattern == action {
			return 0, true
		}
		return 0, false
	}
	if pattern == action {
		return 0, true
	}
	if strings.HasSuffix(action, ":"+pattern) {
		return 1, true
	}
	return 0, false
}

// Lookup returns the winning rule for an action id, if any.
func (r *Rules) Lookup(action string) (mode, pattern string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bestRank, bestLen := 99, -1
	for p, m := range r.rules {
		rank, hit := matchRank(p, action)
		if !hit {
			continue
		}
		lit := len(strings.ReplaceAll(strings.ReplaceAll(p, "*", ""), "?", ""))
		if rank < bestRank || (rank == bestRank && (lit > bestLen || (lit == bestLen && p < pattern))) {
			bestRank, bestLen, mode, pattern, ok = rank, lit, m, p, true
		}
	}
	return mode, pattern, ok
}

// Mode is Lookup for the connector gate's Policy.Rule: "" when no rule applies.
func (r *Rules) Mode(action string) string {
	m, _, ok := r.Lookup(action)
	if !ok {
		return ""
	}
	return m
}

// Set adds or replaces a rule and persists the set.
func (r *Rules) Set(pattern, mode string) error {
	pattern = strings.TrimSpace(pattern)
	if err := validRulePattern(pattern); err != nil {
		return err
	}
	pm, ok := ParseMode(mode)
	if !ok {
		return fmt.Errorf("mode must be ask|allow|deny")
	}
	r.mu.Lock()
	r.rules[pattern] = pm.String()
	snap := r.snapshotLocked()
	persist := r.persist
	r.mu.Unlock()
	r.log.Info("approval rule set", "rule", pattern, "mode", pm.String(), "kind", ruleKind(pattern))
	if persist != nil {
		persist(snap)
	}
	return nil
}

// Remove deletes a rule; false when there was none.
func (r *Rules) Remove(pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	r.mu.Lock()
	_, had := r.rules[pattern]
	delete(r.rules, pattern)
	snap := r.snapshotLocked()
	persist := r.persist
	r.mu.Unlock()
	if !had {
		return false
	}
	r.log.Info("approval rule removed", "rule", pattern)
	if persist != nil {
		persist(snap)
	}
	return true
}

// NormalizeRules trims and validates a raw pattern → mode map (as read from a
// file), returning the usable entries and how many were skipped.
func NormalizeRules(rules map[string]string) (map[string]string, int) {
	next := map[string]string{}
	skipped := 0
	for p, m := range rules {
		pm, ok := ParseMode(m)
		if validRulePattern(strings.TrimSpace(p)) != nil || !ok {
			skipped++
			continue
		}
		next[strings.TrimSpace(p)] = pm.String()
	}
	return next, skipped
}

// Load replaces the whole set WITHOUT persisting (settings load, the file
// watcher). Invalid entries are skipped and counted in the return.
func (r *Rules) Load(rules map[string]string) (loaded, skipped int) {
	next, skipped := NormalizeRules(rules)
	r.mu.Lock()
	r.rules = next
	r.mu.Unlock()
	return len(next), skipped
}

func (r *Rules) snapshotLocked() map[string]string {
	out := make(map[string]string, len(r.rules))
	for p, m := range r.rules {
		out[p] = m
	}
	return out
}

// Snapshot copies the rule set (pattern → mode).
func (r *Rules) Snapshot() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshotLocked()
}

// Rules lists the set in precedence order (exact, then names, then globs;
// longer literals first) — the order a reader should think about them in.
func (r *Rules) Rules() []control.ToolRuleInfo {
	snap := r.Snapshot()
	out := make([]control.ToolRuleInfo, 0, len(snap))
	for p, m := range snap {
		out = append(out, control.ToolRuleInfo{Pattern: p, Mode: m, Kind: ruleKind(p)})
	}
	rank := map[string]int{"exact": 0, "name": 1, "glob": 2}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		li, lj := len(strings.Trim(out[i].Pattern, "*?")), len(strings.Trim(out[j].Pattern, "*?"))
		if li != lj {
			return li > lj
		}
		return out[i].Pattern < out[j].Pattern
	})
	return out
}
