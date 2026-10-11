package main

// judgectl.go is the cmd/tenant adapter behind the TUI `/judge` command and the
// dashboard Judge control (TEN-91): choose which model grades eval answers. It
// reads/writes the persisted override (improve.judge*) and validates the chosen
// provider kind against the catalog. The API key is NEVER entered or stored
// here — it's read from the kind's env var at eval time; Set only records which
// model/kind/endpoint/key-env to use, and warns if the key isn't present yet.

import (
	"fmt"
	"os"
	"strings"

	"tenant/internal/tui"
)

// judgeCtl satisfies tui.JudgeControl. planner is the active daily/planner model
// id (for the "self-judging" display when no override is set).
type judgeCtl struct {
	cfgDir  string
	planner string
}

var _ tui.JudgeControl = judgeCtl{}

// resolveKeyEnv returns the env var the judge key is read from for a kind:
// the persisted JudgeKeyEnv if set, else the catalog default for the kind.
func resolveJudgeKeyEnv(kind, persisted string) string {
	if persisted != "" {
		return persisted
	}
	if pk, ok := providerKinds[kind]; ok && pk.KeyEnv != "" {
		return pk.KeyEnv
	}
	return ""
}

// judgeSetting is the persisted judge, resolved as every reader shows it:
// the kind defaulted, and the key variable it reads with whether it is set.
// ok is false when no separate judge is set (the planner grades).
type judgeSetting struct {
	kind, model, endpoint, keyEnv string
	keyMissing                    bool
}

func (j judgeCtl) resolved() (js judgeSetting, ok bool) {
	lc, err := loadLaunchConfig(j.cfgDir)
	if err != nil || lc.Improve.QualityModel == "" {
		return js, false
	}
	ic := lc.Improve
	js = judgeSetting{kind: ic.QualityModelKind, model: ic.QualityModel, endpoint: ic.QualityModelEndpoint}
	if js.kind == "" {
		js.kind = "anthropic"
	}
	js.keyEnv = resolveJudgeKeyEnv(js.kind, ic.QualityModelKeyEnv)
	js.keyMissing = js.keyEnv != "" && os.Getenv(js.keyEnv) == ""
	return js, true
}

func (j judgeCtl) Current() string {
	if js, ok := j.resolved(); ok {
		keyState := "key ready"
		if js.keyMissing {
			keyState = "⚠ $" + js.keyEnv + " not set"
		}
		ep := js.endpoint
		if ep == "" {
			ep = "default endpoint"
		}
		return fmt.Sprintf("judge: %s · %q (%s · %s) — applies to `tenant eval` + the nightly gate. /judge clear for the planner default.",
			js.kind, js.model, ep, keyState)
	}
	pm := j.planner
	if pm == "" {
		pm = "the planner model"
	}
	return fmt.Sprintf("judge: default — %q grades answers (self-judging, zero setup). /judge set <kind> <model> for a separate judge.", pm)
}

func (j judgeCtl) Set(kind, mdl, endpoint string) (string, error) {
	kind = strings.TrimSpace(kind)
	mdl = strings.TrimSpace(mdl)
	endpoint = strings.TrimSpace(endpoint)
	if kind == "" || mdl == "" {
		return "", fmt.Errorf("need a provider kind and a model id (e.g. anthropic claude-opus-4-8)")
	}
	pk, ok := providerKinds[kind]
	if !ok {
		return "", fmt.Errorf("unknown provider kind %q (known: %s)", kind, strings.Join(providerOrder, ", "))
	}
	if !pk.Wired {
		return "", fmt.Errorf("%s is in the catalog but its backend isn't implemented", pk.Label)
	}
	lc, err := loadLaunchConfig(j.cfgDir)
	if err != nil {
		return "", err
	}
	lc.Improve.QualityModel = mdl
	lc.Improve.QualityModelKind = kind
	lc.Improve.QualityModelEndpoint = endpoint
	lc.Improve.QualityModelKeyEnv = pk.KeyEnv // catalog default; "" for local/no-key kinds
	if err := lc.Save(j.cfgDir); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("judge set → %s model %q.", kind, mdl)
	if pk.KeyEnv != "" && os.Getenv(pk.KeyEnv) == "" {
		msg += fmt.Sprintf(" ⚠ set $%s before the next eval — the judge needs it.", pk.KeyEnv)
	}
	return msg, nil
}

func (j judgeCtl) Clear() error {
	lc, err := loadLaunchConfig(j.cfgDir)
	if err != nil {
		return err
	}
	lc.Improve.QualityModel = ""
	lc.Improve.QualityModelKind = ""
	lc.Improve.QualityModelEndpoint = ""
	lc.Improve.QualityModelKeyEnv = ""
	return lc.Save(j.cfgDir)
}
