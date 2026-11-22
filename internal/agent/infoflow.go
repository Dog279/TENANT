package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"tenant/internal/model"
)

const (
	infoFlowRecentResults    = 16
	infoFlowFragmentsPerRead = 64
	// infoFlowMaxResultBytes bounds a sensitive result both where the tracker
	// records it and where it re-enters the model's context (contextView), so
	// every byte of it the model sees is tracked (TEN-336).
	infoFlowMaxResultBytes   = 1 << 20
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
		if labels[i].Tracked() {
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
	t.adopt([]sensitiveFlowResult{{label: *sensitive, fragments: fragments}})
}

// adopt appends recorded results, keeping only the newest
// infoFlowRecentResults. Results arrive from record or from another agent's
// Flow (TEN-336).
func (t *infoFlowTracker) adopt(results []sensitiveFlowResult) {
	if len(results) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recent = append(t.recent, results...)
	if drop := len(t.recent) - infoFlowRecentResults; drop > 0 {
		copy(t.recent, t.recent[drop:])
		t.recent = t.recent[:infoFlowRecentResults]
	}
}

func (t *infoFlowTracker) match(args json.RawMessage) (model.InfoFlowLabel, bool) {
	matched := t.quoting(outboundArgumentForms(args))
	if len(matched) == 0 {
		return model.InfoFlowLabel{}, false
	}
	// One approval speaks for the whole call, so it names the strictest thing
	// the call quotes: a vault secret over whatever was read after it.
	for _, m := range matched {
		if m.label.Provenance == model.ProvenanceVault {
			return m.label, true
		}
	}
	return matched[0].label, true
}

// quotedByArgs returns every recorded sensitive result a tool call's
// arguments quote, newest first.
func (t *infoFlowTracker) quotedByArgs(args json.RawMessage) []sensitiveFlowResult {
	return t.quoting(outboundArgumentForms(args))
}

// quotedByText returns every recorded sensitive result text quotes, newest
// first.
func (t *infoFlowTracker) quotedByText(text string) []sensitiveFlowResult {
	var forms flowForms
	forms.add(text)
	return t.quoting(forms.out)
}

func (t *infoFlowTracker) quoting(forms []string) []sensitiveFlowResult {
	if len(forms) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []sensitiveFlowResult
	for i := len(t.recent) - 1; i >= 0; i-- {
		if quotesAny(t.recent[i].fragments, forms) {
			out = append(out, t.recent[i])
		}
	}
	return out
}

func quotesAny(fragments, forms []string) bool {
	for _, fragment := range fragments {
		longFragment := utf8.RuneCountInString(fragment) >= infoFlowMinFragmentRunes
		for _, form := range forms {
			longForm := utf8.RuneCountInString(form) >= infoFlowMinFragmentRunes
			if (longFragment && strings.Contains(form, fragment)) ||
				(longForm && strings.Contains(fragment, form)) ||
				form == fragment {
				return true
			}
		}
	}
	return false
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

// flowForms collects the normalized, deduplicated forms an outbound string
// is compared in: the string, its percent-decoded variants, and their tokens.
type flowForms struct {
	seen map[string]bool
	out  []string
}

func (f *flowForms) add(s string) {
	for _, candidate := range decodedFlowForms(s) {
		f.append(candidate)
		for _, token := range strings.FieldsFunc(candidate, func(r rune) bool {
			return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-')
		}) {
			f.append(token)
		}
	}
}

func (f *flowForms) append(candidate string) {
	candidate = normalizeFlowText(candidate)
	if candidate == "" || f.seen[candidate] {
		return
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	f.seen[candidate] = true
	f.out = append(f.out, candidate)
}

func outboundArgumentForms(args json.RawMessage) []string {
	var forms flowForms
	forms.add(string(args))
	var decoded any
	if json.Unmarshal(args, &decoded) == nil {
		walkJSONStrings(decoded, forms.add)
	}
	return forms.out
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

// decodedFlowForms returns s with up to two rounds of percent-decoding, both
// query-style (+ is a space) and path-style. Decoding is tolerant: valid %HH
// escapes decode even when the string also holds malformed ones, so a single
// "%zz" cannot hide an otherwise encoded secret (TEN-336).
func decodedFlowForms(s string) []string {
	out := []string{s}
	for _, plusAsSpace := range []bool{true, false} {
		cur := s
		for range 2 {
			next := percentDecode(cur, plusAsSpace)
			if next == cur {
				break
			}
			out = append(out, next)
			cur = next
		}
	}
	return out
}

// percentDecode decodes every valid %HH escape in s and keeps anything
// malformed as written. With plusAsSpace, + decodes to a space.
func percentDecode(s string, plusAsSpace bool) string {
	if !strings.ContainsRune(s, '%') && (!plusAsSpace || !strings.ContainsRune(s, '+')) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]):
			b.WriteByte(hexValue(s[i+1])<<4 | hexValue(s[i+2]))
			i += 2
		case c == '+' && plusAsSpace:
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func hexValue(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	default:
		return c - '0'
	}
}

func normalizeFlowText(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Flow is sensitive-data provenance handed from one agent to another with a
// delegated task or a returned result (TEN-336): the sensitive results the
// sender recorded that the text quotes. The receiving agent tracks them like
// its own sensitive reads, so its egress of that data asks for approval too.
// The zero value carries nothing.
type Flow struct{ results []sensitiveFlowResult }

// NewFlow is provenance for text that reaches an agent other than as a tool
// result, such as the log events attached to an alert's investigation
// (TEN-400). Seeded as Config.InboundFlow, the text is tracked like a
// labelled result: an outbound call quoting it asks first, and FlowFor finds
// it in what the agent writes.
func NewFlow(label model.InfoFlowLabel, content string) Flow {
	var t infoFlowTracker
	t.record([]model.InfoFlowLabel{label}, content)
	return Flow{results: t.recent}
}

// Empty reports whether f carries no sensitive provenance.
func (f Flow) Empty() bool { return len(f.results) == 0 }

// Labels returns f's distinct labels, oldest first.
func (f Flow) Labels() []model.InfoFlowLabel {
	var out []model.InfoFlowLabel
	for i := len(f.results) - 1; i >= 0; i-- {
		out = appendLabel(out, f.results[i].label)
	}
	return out
}

// JoinFlows combines flows, for a result assembled from several agents.
func JoinFlows(flows ...Flow) Flow {
	var out Flow
	for _, f := range flows {
		out.results = append(out.results, f.results...)
	}
	return out
}

func appendLabel(labels []model.InfoFlowLabel, label model.InfoFlowLabel) []model.InfoFlowLabel {
	for _, have := range labels {
		if have == label {
			return labels
		}
	}
	return append(labels, label)
}

type dispatchFlowKey struct{}

type flowReportKey struct{}

// flowReport collects what a dispatched tool hands back through ReportFlow.
type flowReport struct {
	mu   sync.Mutex
	flow Flow
}

func (r *flowReport) add(f Flow) {
	r.mu.Lock()
	r.flow = JoinFlows(r.flow, f)
	r.mu.Unlock()
}

func (r *flowReport) get() Flow {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flow
}

// DispatchFlow returns the provenance the calling agent attached to the tool
// call being dispatched: the sensitive results its arguments quote, attached
// once the operator approved that flow. A tool that starts another agent
// passes it on as Config.InboundFlow.
func DispatchFlow(ctx context.Context) Flow {
	f, _ := ctx.Value(dispatchFlowKey{}).(Flow)
	return f
}

// ReportFlow hands provenance back to the agent whose tool call is being
// dispatched, for a tool whose result carries another agent's output. The
// calling agent tracks it and labels the result. A no-op outside a dispatch.
func ReportFlow(ctx context.Context, f Flow) {
	if r, ok := ctx.Value(flowReportKey{}).(*flowReport); ok && !f.Empty() {
		r.add(f)
	}
}
