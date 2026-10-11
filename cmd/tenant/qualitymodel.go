package main

// qualitymodel.go is the cmd/tenant adapter behind the TUI `/eval model`
// command and the dashboard Quality page's grader card (TEN-91, renamed in
// TEN-516): the Quality model is the model that grades eval answers. It
// reads/writes the saved choice (improve.quality_model*) and validates the
// chosen provider kind against the catalog. The API key is NEVER entered or
// stored here — it's read from the kind's env var at eval time; Set only
// records which model/kind/endpoint/key-env to use, and warns if the key
// isn't present yet.

import (
	"fmt"
	"os"
	"strings"

	"tenant/internal/tui"
)

// qualityModelCtl satisfies tui.QualityModelControl. planner is the active
// daily/planner model id (for the "grades its own answers" display when no
// separate model is set).
type qualityModelCtl struct {
	cfgDir  string
	planner string
}

var _ tui.QualityModelControl = qualityModelCtl{}

// resolveQualityKeyEnv returns the env var the Quality model's key is read
// from for a kind: the saved one if set, else the catalog default for the kind.
func resolveQualityKeyEnv(kind, persisted string) string {
	if persisted != "" {
		return persisted
	}
	if pk, ok := providerKinds[kind]; ok && pk.KeyEnv != "" {
		return pk.KeyEnv
	}
	return ""
}

// qualitySetting is the saved Quality model, resolved as every reader shows
// it: the kind defaulted, and the key variable it reads with whether it is
// set. ok is false when no separate model is set (the planner grades).
type qualitySetting struct {
	kind, model, endpoint, keyEnv string
	keyMissing                    bool
}

func (j qualityModelCtl) resolved() (js qualitySetting, ok bool) {
	lc, err := loadLaunchConfig(j.cfgDir)
	if err != nil || lc.Improve.QualityModel == "" {
		return js, false
	}
	ic := lc.Improve
	js = qualitySetting{kind: ic.QualityModelKind, model: ic.QualityModel, endpoint: ic.QualityModelEndpoint}
	if js.kind == "" {
		js.kind = "anthropic"
	}
	js.keyEnv = resolveQualityKeyEnv(js.kind, ic.QualityModelKeyEnv)
	js.keyMissing = js.keyEnv != "" && os.Getenv(js.keyEnv) == ""
	return js, true
}

func (j qualityModelCtl) Current() string {
	if js, ok := j.resolved(); ok {
		keyState := "key ready"
		if js.keyMissing {
			keyState = "⚠ $" + js.keyEnv + " not set"
		}
		ep := js.endpoint
		if ep == "" {
			ep = "default endpoint"
		}
		return fmt.Sprintf("Quality model: %s · %q (%s · %s) — grades eval answers for `tenant eval` and the nightly check. /eval model clear for the default.",
			js.kind, js.model, ep, keyState)
	}
	pm := j.planner
	if pm == "" {
		pm = "the planner model"
	}
	return fmt.Sprintf("Quality model: default — %q grades its own answers (zero setup). /eval model set <kind> <model> for a separate grader.", pm)
}

func (j qualityModelCtl) Set(kind, mdl, endpoint string) (string, error) {
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
	msg := fmt.Sprintf("Quality model set → %s %q grades eval answers.", kind, mdl)
	if pk.KeyEnv != "" && os.Getenv(pk.KeyEnv) == "" {
		msg += fmt.Sprintf(" ⚠ set $%s before the next eval — the Quality model needs it.", pk.KeyEnv)
	}
	return msg, nil
}

func (j qualityModelCtl) Clear() error {
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
