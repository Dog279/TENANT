package route

import (
	"errors"
	"fmt"
	"strings"
)

// Dataset export (TEN-292): decisions × episode outcomes → Supra-format
// training rows. The decision log stores prompt hashes only, so the caller
// (cmd/tenant) resolves prompt text through the episodic join and this file
// stays pure: label derivation + row formatting, no I/O.

const (
	// Gold rows come from explicit operator judgment (!big overrides and
	// confirmed assisted suggestions); derived rows from outcome inference.
	ExportWeightGold    = 3.0
	ExportWeightDerived = 1.0

	ExportSourceGold    = "gold"
	ExportSourceDerived = "derived"

	// ExportLabelSmall/Big are the exact training-frame label strings.
	ExportLabelSmall = "small model"
	ExportLabelBig   = "big model"
)

// TrainingRow is one JSONL record of the exported dataset. Text embeds the
// full completion frame (prompt + analysis + label); Label repeats the routed
// verdict for tooling that filters without re-parsing Text.
type TrainingRow struct {
	Text       string  `json:"text"`
	Label      string  `json:"label"`
	Weight     float64 `json:"weight"`
	Source     string  `json:"source"`
	DecisionID string  `json:"decision_id,omitempty"`
	PromptHash string  `json:"prompt_hash,omitempty"`
}

// ExportJoin is one decision joined to its completed episode. Prompt is the
// episode's stored prompt text (the decision itself only has the hash);
// Outcome is the episode outcome ("success" | "error" | "unknown" | "");
// Applied reports whether the routed selection actually served the turn (the
// route:applied episode tag) — in shadow mode the planner ran regardless of
// what the decision selected.
type ExportJoin struct {
	Decision Decision
	Prompt   string
	Outcome  string
	Applied  bool
}

// ErrExportSkip marks a join that produces no training row (not an error
// condition — e.g. the classifier never parsed, so there is no analysis frame).
var ErrExportSkip = errors.New("route: export: no training row for decision")

// DeriveRow converts one joined observation into a training row using the
// TEN-292 outcome-derived labeling policy:
//
//   - operator overrides and confirmed assisted suggestions → gold "big"
//   - small ran and the turn succeeded → derived "small"
//     (this includes shadow big-labels over successful local turns — the
//     ticket's "shadow big-label correction")
//   - small ran and the turn failed (episode error, or a failover/loop-ceiling
//     proxy recorded as an error outcome) → derived "big"
//   - big ran and succeeded → derived "big" (confirmation)
//   - big ran and failed → skipped: a failed big turn labels neither tier
//
// Rows require a parsed classifier signal: without Domain/Complexity there is
// no analysis frame to train on, so those joins return ErrExportSkip.
func DeriveRow(j ExportJoin) (TrainingRow, error) {
	d := j.Decision
	if strings.TrimSpace(j.Prompt) == "" {
		return TrainingRow{}, fmt.Errorf("%w: prompt text unavailable", ErrExportSkip)
	}
	if d.Status != StatusOK || d.Label == "" || d.Domain == "" {
		return TrainingRow{}, fmt.Errorf("%w: no parsed classifier signal", ErrExportSkip)
	}

	label, weight, source, err := deriveLabel(d, j.Outcome, j.Applied)
	if err != nil {
		return TrainingRow{}, err
	}
	return TrainingRow{
		Text:       FormatTrainingText(j.Prompt, d.Signal(), label),
		Label:      label,
		Weight:     weight,
		Source:     source,
		DecisionID: d.ID,
		PromptHash: d.PromptHash,
	}, nil
}

func deriveLabel(d Decision, outcome string, applied bool) (string, float64, string, error) {
	// Explicit operator judgment is gold regardless of outcome. Known
	// limitation: a bare !big override skips classification entirely, so it
	// has no analysis frame and never reaches this function — today's gold
	// rows come from confirmed assisted suggestions (which did classify).
	if d.PolicyReason == string(ReasonOperatorBig) || d.OperatorConfirmed {
		return ExportLabelBig, ExportWeightGold, ExportSourceGold, nil
	}

	// What RAN is what teaches: in shadow mode the selection was never
	// applied, so the local planner served the turn no matter what tier the
	// policy picked. A shadow big-selection over a successful local turn is
	// exactly the ticket's "small correction".
	ranBig := applied && d.SelectedTier == TierBig
	failed := strings.TrimSpace(outcome) == TurnOutcomeError
	switch {
	case !ranBig && !failed:
		return ExportLabelSmall, ExportWeightDerived, ExportSourceDerived, nil
	case !ranBig && failed:
		return ExportLabelBig, ExportWeightDerived, ExportSourceDerived, nil
	case ranBig && !failed:
		return ExportLabelBig, ExportWeightDerived, ExportSourceDerived, nil
	default:
		return "", 0, "", fmt.Errorf("%w: failed big turn labels neither tier", ErrExportSkip)
	}
}

// FormatTrainingText renders the exact completion frame Supra was trained on,
// substituting the derived label for the model's original verdict. Flags use
// the checkpoint schema's True/False form.
func FormatTrainingText(prompt string, sig Signal, label string) string {
	justification := sig.Justification
	if strings.TrimSpace(justification) == "" {
		justification = "outcome-derived label"
	}
	return fmt.Sprintf("Task: %s\nAnalysis: Domain: %s | Complexity: %d | Math: %s | Code: %s | Route: %s | Justification: %s",
		prompt, sig.Domain, sig.Complexity, exportFlag(sig.Math), exportFlag(sig.Code), label, justification)
}

func exportFlag(v bool) string {
	if v {
		return "True"
	}
	return "False"
}
