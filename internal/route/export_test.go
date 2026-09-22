package route

import (
	"errors"
	"strings"
	"testing"
)

func okDecision() Decision {
	return Decision{
		ID: "d1", PromptHash: "abc", Status: StatusOK,
		Domain: "software", Complexity: 4, Math: false, Code: true,
		Label: "big", Justification: "multi-step refactor",
		SelectedTier: TierSmall, PolicyReason: string(ReasonMinComplexity),
		Mode: ModeShadow,
	}
}

func TestDeriveRowLabelPolicy(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Decision)
		outcome    string
		applied    bool
		wantLabel  string
		wantWeight float64
		wantSource string
		wantSkip   bool
	}{
		{
			name:    "small ran and succeeded → small",
			mutate:  func(d *Decision) { d.SelectedTier = TierSmall },
			outcome: "success", applied: true,
			wantLabel: ExportLabelSmall, wantWeight: ExportWeightDerived, wantSource: ExportSourceDerived,
		},
		{
			name:    "shadow big-label over successful local turn → small correction",
			mutate:  func(d *Decision) { d.SelectedTier = TierSmall; d.Label = "big" },
			outcome: "success", applied: false,
			wantLabel: ExportLabelSmall, wantWeight: ExportWeightDerived, wantSource: ExportSourceDerived,
		},
		{
			// The verification workflow's #8: a shadow decision that SELECTED
			// big but never ran it must still export as a small correction
			// when the local turn succeeded.
			name:    "shadow big-SELECTION over successful local turn → small correction",
			mutate:  func(d *Decision) { d.SelectedTier = TierBig; d.Label = "big" },
			outcome: "success", applied: false,
			wantLabel: ExportLabelSmall, wantWeight: ExportWeightDerived, wantSource: ExportSourceDerived,
		},
		{
			name:    "small ran and failed → big",
			mutate:  func(d *Decision) { d.SelectedTier = TierSmall },
			outcome: TurnOutcomeError, applied: true,
			wantLabel: ExportLabelBig, wantWeight: ExportWeightDerived, wantSource: ExportSourceDerived,
		},
		{
			name:    "big ran and succeeded → big confirmation",
			mutate:  func(d *Decision) { d.SelectedTier = TierBig },
			outcome: "success", applied: true,
			wantLabel: ExportLabelBig, wantWeight: ExportWeightDerived, wantSource: ExportSourceDerived,
		},
		{
			name:    "big ran and failed → labels neither tier",
			mutate:  func(d *Decision) { d.SelectedTier = TierBig },
			outcome: TurnOutcomeError, applied: true,
			wantSkip: true,
		},
		{
			name:    "operator !big is gold regardless of outcome",
			mutate:  func(d *Decision) { d.PolicyReason = string(ReasonOperatorBig); d.SelectedTier = TierBig },
			outcome: TurnOutcomeError, applied: true,
			wantLabel: ExportLabelBig, wantWeight: ExportWeightGold, wantSource: ExportSourceGold,
		},
		{
			name:    "confirmed assisted suggestion is gold",
			mutate:  func(d *Decision) { d.OperatorConfirmed = true },
			outcome: "success", applied: false,
			wantLabel: ExportLabelBig, wantWeight: ExportWeightGold, wantSource: ExportSourceGold,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := okDecision()
			tt.mutate(&d)
			row, err := DeriveRow(ExportJoin{Decision: d, Prompt: "refactor the auth handler", Outcome: tt.outcome, Applied: tt.applied})
			if tt.wantSkip {
				if !errors.Is(err, ErrExportSkip) {
					t.Fatalf("DeriveRow() err = %v, want ErrExportSkip", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeriveRow() err = %v", err)
			}
			if row.Label != tt.wantLabel || row.Weight != tt.wantWeight || row.Source != tt.wantSource {
				t.Fatalf("row = {label:%q weight:%v source:%q}, want {%q %v %q}",
					row.Label, row.Weight, row.Source, tt.wantLabel, tt.wantWeight, tt.wantSource)
			}
			if !strings.Contains(row.Text, "Route: "+tt.wantLabel) {
				t.Fatalf("row text does not carry derived label: %q", row.Text)
			}
		})
	}
}

func TestDeriveRowSkipsWithoutSignalOrPrompt(t *testing.T) {
	noSignal := okDecision()
	noSignal.Status = StatusError
	if _, err := DeriveRow(ExportJoin{Decision: noSignal, Prompt: "p", Outcome: "success"}); !errors.Is(err, ErrExportSkip) {
		t.Fatalf("unparsed decision: err = %v, want ErrExportSkip", err)
	}
	if _, err := DeriveRow(ExportJoin{Decision: okDecision(), Prompt: "  ", Outcome: "success"}); !errors.Is(err, ErrExportSkip) {
		t.Fatalf("missing prompt: err = %v, want ErrExportSkip", err)
	}
}

// TestFormatTrainingTextRoundTripsThroughParser is the load-bearing property:
// exported rows must be exactly the frame the strict parser (and therefore the
// fine-tune) understands, with the derived label substituted.
func TestFormatTrainingTextRoundTripsThroughParser(t *testing.T) {
	sig := Signal{Domain: "software", Complexity: 4, Math: false, Code: true, Justification: "multi-step refactor"}
	text := FormatTrainingText("refactor the auth handler", sig, ExportLabelBig)

	wantPrefix := "Task: refactor the auth handler\nAnalysis: "
	if !strings.HasPrefix(text, wantPrefix) {
		t.Fatalf("text = %q, want prefix %q", text, wantPrefix)
	}
	completion := strings.TrimPrefix(text, wantPrefix)
	parsed, err := ParseSignal(completion)
	if err != nil {
		t.Fatalf("exported analysis does not re-parse: %v\n%s", err, completion)
	}
	if parsed.Domain != sig.Domain || parsed.Complexity != sig.Complexity ||
		parsed.Math != sig.Math || parsed.Code != sig.Code || parsed.Label != "big" {
		t.Fatalf("round-trip mismatch: %+v", parsed)
	}
}
