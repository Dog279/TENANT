package agent

import (
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"tenant/internal/model"
)

const (
	infoFlowRecentResults    = 16
	infoFlowFragmentsPerRead = 64
	infoFlowMaxResultBytes   = 64 * 1024
	infoFlowMinFragmentRunes = 8
)

type sensitiveFlowResult struct {
	label     model.InfoFlowLabel
	fragments []string
}

// infoFlowTracker retains a bounded, normalized fingerprint of recent
// sensitive results. It deliberately stores no global state: labels follow one
// agent's working context and approval origin.
type infoFlowTracker struct {
	mu     sync.Mutex
	recent []sensitiveFlowResult
}

func newInfoFlowTracker() *infoFlowTracker { return &infoFlowTracker{} }

func (t *infoFlowTracker) record(labels []model.InfoFlowLabel, content string) {
	var sensitive *model.InfoFlowLabel
	for i := range labels {
		if labels[i].Sensitivity == model.SensitivitySensitive {
			label := labels[i]
			sensitive = &label
			break
		}
	}
	if sensitive == nil || content == "" {
		return
	}
	if len(content) > infoFlowMaxResultBytes {
		content = strings.ToValidUTF8(content[:infoFlowMaxResultBytes], "")
	}
	fragments := sensitiveFragments(content)
	if len(fragments) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recent = append(t.recent, sensitiveFlowResult{label: *sensitive, fragments: fragments})
	if drop := len(t.recent) - infoFlowRecentResults; drop > 0 {
		copy(t.recent, t.recent[drop:])
		t.recent = t.recent[:infoFlowRecentResults]
	}
}

func (t *infoFlowTracker) match(args json.RawMessage) (model.InfoFlowLabel, bool) {
	forms := outboundArgumentForms(args)
	if len(forms) == 0 {
		return model.InfoFlowLabel{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.recent) - 1; i >= 0; i-- {
		for _, fragment := range t.recent[i].fragments {
			for _, form := range forms {
				longFragment := utf8.RuneCountInString(fragment) >= infoFlowMinFragmentRunes
				longForm := utf8.RuneCountInString(form) >= infoFlowMinFragmentRunes
				if (longFragment && strings.Contains(form, fragment)) ||
					(longForm && strings.Contains(fragment, form)) ||
					form == fragment {
					return t.recent[i].label, true
				}
			}
		}
	}
	return model.InfoFlowLabel{}, false
}

func sensitiveFragments(content string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, infoFlowFragmentsPerRead)
	add := func(s string, allowShort bool) {
		s = normalizeFlowText(s)
		if s == "" || (!allowShort && utf8.RuneCountInString(s) < infoFlowMinFragmentRunes) || seen[s] || len(out) >= infoFlowFragmentsPerRead {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(content, true)
	for _, line := range strings.Split(content, "\n") {
		add(line, true)
		for _, token := range strings.FieldsFunc(line, func(r rune) bool {
			return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-')
		}) {
			add(token, false)
		}
	}
	return out
}

func outboundArgumentForms(args json.RawMessage) []string {
	seen := map[string]bool{}
	var out []string
	appendForm := func(candidate string) {
		candidate = normalizeFlowText(candidate)
		if candidate != "" && !seen[candidate] {
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	add := func(s string) {
		for _, candidate := range decodedFlowForms(s) {
			appendForm(candidate)
			for _, token := range strings.FieldsFunc(candidate, func(r rune) bool {
				return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-')
			}) {
				appendForm(token)
			}
		}
	}
	add(string(args))
	var decoded any
	if json.Unmarshal(args, &decoded) == nil {
		walkJSONStrings(decoded, add)
	}
	return out
}

func walkJSONStrings(v any, add func(string)) {
	switch x := v.(type) {
	case string:
		add(x)
	case []any:
		for _, item := range x {
			walkJSONStrings(item, add)
		}
	case map[string]any:
		for _, item := range x {
			walkJSONStrings(item, add)
		}
	}
}

func decodedFlowForms(s string) []string {
	out := []string{s}
	for _, decode := range []func(string) (string, error){url.QueryUnescape, url.PathUnescape} {
		cur := s
		for range 2 {
			next, err := decode(cur)
			if err != nil || next == cur {
				break
			}
			out = append(out, next)
			cur = next
		}
	}
	return out
}

func normalizeFlowText(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
