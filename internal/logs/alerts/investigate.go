package alerts

// investigate.go is what an alert's investigation (the Investigate action,
// TEN-400) is given, and its Security records. The composition root runs it
// (cmd/tenant/loginvestigate.go): the operator approves each one, the turn is
// read-only and within a budget, and this text reaches it labelled untrusted,
// since anything can write a log (docs/design/logging-v2.md §11).

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

const (
	// attachMax is how many events an investigation is given: the alert's
	// and the nearest of its turn's or activity's.
	attachMax = 40
	// attachBytes cuts the rendered events, whatever their number.
	attachBytes = 16 << 10
	// idMax and keyMax cut a correlation ID and a field name, which an
	// outside writer may have set.
	idMax, keyMax = 64, 40
	// resultMax cuts an investigation's result where it leaves the log book.
	resultMax = 1500
)

// Investigations is the Security log's record of the investigations alert
// rules start: one that did not start, and one that ran, with its turn's
// trace, so trace:<id> finds the turn and runs.db its record.
var Investigations = event.NewProvider("investigate", "security")

var InvestigationStarted = Investigations.Event(4001, event.Info, "Investigation", "Investigation of alert {rule} started as turn {trace}, approved by the operator", "rule", "seq", "trace").
	Keywords("audit.success").
	Doc("The operator approved an alert rule's investigate action, and an agent turn began looking into the event: read-only tools, within its budget, the log text labelled untrusted. seq is the alert's event.",
		"An alert rule with action.kind investigate fired, and the operator approved the request.",
		"None. trace:<trace> follows the turn; investigate 4003 is its result.")

var InvestigationNotStarted = Investigations.Event(4002, event.Warning, "Investigation", "Investigation of alert {rule} not started: {reason}", "rule", "seq", "reason").
	Doc("An alert rule's investigate action asked the operator to approve an agent investigation, and no turn ran.",
		"The operator denied it, nobody answered in time, Tenant stopped first, no surface could ask the operator, or the event could not be read.",
		"None when intended. To be asked less often, raise the rule's throttle or narrow its filter in logs.alerts.")

var InvestigationDone = Investigations.Event(4003, event.Info, "Investigation", "Investigation {trace} of alert {rule} {outcome}: {steps} steps, {tool_calls} tool calls, {tokens} tokens", "rule", "seq", "trace", "outcome", "steps", "tool_calls", "tokens", "result").
	Sensitive("result").
	Doc("An alert's investigation ended. result is the agent's report, masked until revealed: it is written from log text anyone can write, and may quote memory. The same report is on the status feed and, when the rule names one, its channel, unless it quotes sensitive data.",
		"An approved investigation finished, reached its budget or step limit, or failed.",
		"Read the result, and check what it claims against the events (trace:<trace>, and the alert's event, seq): it can be wrong, or steered by a crafted log line.")

// Attachment is what an investigation of a is given: a's event and the
// events of its turn or activity nearest it (store.Timeline), at most
// attachMax, rendered by Lines.
func Attachment(ctx context.Context, st *store.Store, a Alert) (string, error) {
	e, err := st.Get(ctx, a.Seq)
	if err != nil {
		return "", err
	}
	evs := []event.Event{e}
	if id := cmp.Or(e.Corr.TraceID, e.Corr.ActivityID); id != "" {
		tl, err := st.Timeline(ctx, id, store.MaxTimelineDepth)
		if err != nil {
			return "", err
		}
		if i := slices.IndexFunc(tl, func(t event.Event) bool { return t.Seq == e.Seq }); i >= 0 {
			lo := max(0, min(i-attachMax/2, len(tl)-attachMax))
			evs = tl[lo:min(len(tl), lo+attachMax)]
		}
	}
	return Lines(evs), nil
}

// Lines renders events for an investigation, one line each: the time, level,
// number, provider/ID, log and correlation IDs, the message, and the
// non-sensitive fields. A sensitive field is left out, name and all (§11).
// Every part is scrubbed again, put on one line and cut, and the whole line
// scrubbed once more, for a secret split across a key and its value; the
// whole is cut at attachBytes.
func Lines(evs []event.Event) string {
	var b strings.Builder
	for i, e := range evs {
		var l strings.Builder
		fmt.Fprintf(&l, "%s %s seq=%d %s/%d log=%s", e.Time.UTC().Format(time.RFC3339), e.Level, e.Seq, name(e.Provider), e.ID, name(e.Log))
		for _, c := range [][2]string{{"trace", e.Corr.TraceID}, {"activity", e.Corr.ActivityID}, {"related", e.Corr.RelatedID}} {
			if c[1] != "" {
				fmt.Fprintf(&l, " %s=%s", c[0], clip(c[1], idMax))
			}
		}
		fmt.Fprintf(&l, ": %s", clip(logs.Scrub(e.Msg), msgMax))
		for _, f := range e.Data {
			if !f.Sensitive {
				fmt.Fprintf(&l, " | %s=%s", clip(f.Key, keyMax), clip(logs.Scrub(f.Text()), msgMax))
			}
		}
		line := logs.Scrub(l.String())
		if b.Len()+len(line) >= attachBytes {
			fmt.Fprintf(&b, "… %d more events cut\n", len(evs)-i)
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// Investigated is a as its investigation's result is sent to its channel
// (Target): the facts, and in place of the message, the outcome and text,
// scrubbed again, on one line and cut, which Text puts in a code span.
func (a Alert) Investigated(outcome, text string) Alert {
	a.Action, a.Target = a.Target, ""
	a.Msg = clip(logs.Scrub("investigation "+outcome+": "+text), resultMax)
	return a
}
