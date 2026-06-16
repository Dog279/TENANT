package main

// skillctl is the runtime control surface for skill configuration —
// the `/skill configure` flow that mirrors `/model add-cloud`. The
// catalog (`skillKinds`) is empty in production until per-platform
// tickets (TEN-65+) populate it; the framework here is exercised
// against a fake catalog in skillctl_test.go.
//
// NO package-level init() lives in this file by design. The catalog is
// a plain map literal that tests can replace via newSkillControl's
// `kinds` argument. See TEN-64 audit P2.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"tenant/internal/tui"
)

// skillKind is the static catalog metadata for one configurable
// integration. Mirrors providerKind (launchconfig.go:133) in shape.
//
// Per-platform tickets (TEN-65..69) register entries in skillKinds
// with their own Validate + Probe. This file ships the framework with
// an EMPTY catalog so the slash command surface can be tested without
// any platform side effects.
type skillKind struct {
	ID        string
	Label     string
	Fields    []skillKindField
	Probe     skillProbe // nil ⇒ no liveness check
	SetupHint string     // shown after successful configure
	Wired     bool       // false = catalog entry exists but not implemented
	// ProbeTimeout overrides the default 5-second probe timeout. Skills
	// with interactive auth paths (TEN-71 gsuite oauth opens a browser
	// for up to several minutes) bump this; everything else inherits
	// the safe default.
	ProbeTimeout time.Duration
}

// skillProbe is the optional liveness check called after Configure +
// from Probe. MUST honor ctx.Done() — runProbe enforces a 5s timeout
// via context, but a probe that ignores ctx will leak a goroutine
// (the runProbe wrapper returns timeout-error eagerly; the goroutine
// is logged but allowed to leak knowingly).
//
// Returns a human-readable identity ("MyBotName#1234", "@username")
// surfaced in the success message.
//
// The save callback (TEN-71): probes that mutate credentials during
// their run (e.g. gsuite oauth caches the freshly-minted refresh
// token) call save() to persist mid-probe. Implementation re-writes
// credentials.json from the current in-memory creds map. Probes that
// don't mutate creds should ignore the callback.
type skillProbe func(ctx context.Context, creds *credentials, settings map[string]string, save func() error) (identity string, err error)

// skillKindField is one configurable field on a skill. Validators are
// pure functions — testable in isolation.
type skillKindField struct {
	Key      string // namespaced "skill:<id>:<key>" in credentials.json when Secret
	Prompt   string
	Secret   bool               // → credentials.json (0600); else lc.Skills[id].Settings
	Required bool               // clearing a required field on an enabled skill auto-disables it
	Validate func(string) error // nil ⇒ no validation
	Default  string
	// Options, if non-empty, enumerates the legal values. Drives the
	// TUI picker for `/configure` (enum field → modal picker; free-text
	// field → chat-input prompt). Validator should still be set (it's
	// the source of truth) — Options is UX metadata.
	Options []string
	// OptionLabels, if non-nil, provides human-readable labels for each
	// option (parallel to Options — same length, same order). The TUI
	// picker displays these instead of the raw option values; the
	// underlying VALUE stored is still from Options. Lets us frame
	// technical values ("oauth") with non-developer-friendly labels
	// ("Sign in with your Google account") without changing config
	// file shape.
	OptionLabels []string
	// ShowIf, if non-nil, hides this field during the interactive
	// `/configure` walkthrough when it returns false. Used for fields
	// that only apply under specific prior selections (e.g. gsuite's
	// sa_json + subject are sa-only). The interactive flow evaluates
	// this AFTER all prior fields are collected — values is the
	// running map of answers so far.
	//
	// Persistence layer (SkillConfigure) does NOT consult ShowIf —
	// operators using the one-shot key=value form can still set the
	// field if they want to.
	ShowIf func(values map[string]string) bool
	// NoteAfter, if non-nil, fires right after the value is collected
	// (interactive flow only). Returns:
	//   - msg: human-readable guidance to print to chat (empty = silent)
	//   - abort: true ⇒ STOP the configure session immediately. The
	//     stored field values DON'T persist; no probe runs. Used for
	//     hard-blocking prerequisites (e.g. gsuite auth=gcloud but the
	//     gcloud CLI isn't installed).
	NoteAfter func(value string) (msg string, abort bool)
}

// skillKinds is the production catalog. Empty in this ticket — per-
// platform tickets (TEN-65 onward) populate entries. NEVER add an
// init() function to populate this; tests inject via newSkillControl.
var skillKinds = map[string]skillKind{}

// skillCfgControl implements tui.SkillConfigControl. One per process.
type skillCfgControl struct {
	mu     sync.Mutex
	cfgDir string

	// kinds is the catalog this control reads from. Production callers
	// pass `skillKinds`; tests pass a fixture. Injected for test
	// isolation (audit P2 — no global mutation in tests).
	kinds map[string]skillKind

	// setPluginEnabled bridges to toolMux.SetPluginEnabled so probe
	// success can auto-enable the plugin. nil-safe for unit tests that
	// don't care about the tool-mux side effect.
	setPluginEnabled func(label string, on bool) (int, string, error)

	// onDiscordConfigured is called after a successful Discord configure+probe
	// so the relay manager can hot-rebuild its agent+gateway with the new token
	// AND auto-start the relay bound to the operator's Discord user ID.
	// Nil-safe; only relevant for the "discord" skill.
	onDiscordConfigured func(token, operatorID string) error
}

// newSkillControl wires a skill control over the operator's config
// dir, a catalog (production: skillKinds; tests: a fixture), and an
// auto-enable bridge.
func newSkillCfgControl(cfgDir string, kinds map[string]skillKind, setPluginEnabled func(string, bool) (int, string, error), mcpConnect ...atlassianMCPConnector) *skillCfgControl {
	if kinds == nil {
		kinds = map[string]skillKind{}
	}
	var connect atlassianMCPConnector
	if len(mcpConnect) > 0 {
		connect = mcpConnect[0]
	}
	// Some catalog entries adapt to the operator's runtime environment
	// (e.g. gsuite hides oauth_creds_json when maintainer-owned embedded
	// OAuth credentials exist under cfgDir). Apply those adaptations
	// once at construction so subsequent SkillFields calls see the
	// resolved catalog.
	if k, ok := kinds["gsuite"]; ok {
		kinds["gsuite"] = adaptGSuiteForCfgDir(k, cfgDir)
	}
	if k, ok := kinds["atlassian"]; ok { // TEN-160/164: cfgDir for native OAuth cache + the MCP connector
		kinds["atlassian"] = adaptAtlassianForCfgDir(k, cfgDir, connect)
	}
	return &skillCfgControl{cfgDir: cfgDir, kinds: kinds, setPluginEnabled: setPluginEnabled}
}

// SkillList returns every skill known to this control. It surfaces BOTH the
// /configure-framework catalog (skillKinds) AND the wizard-only skills
// (wizardLocalKinds, skills_setup.go) so a fresh install where the new catalog
// is empty still gives operators a discoverable list rather than the
// unactionable "(no skills configured)" surface (audit P1).
//
// Wizard-only entries are marked `Legacy: true` and the TUI renders them with
// a "[legacy: use tenant setup]" suffix — they're path/flag-driven (wiki, sql,
// imessage, os) and configured via `tenant setup`, not `/configure`.
func (sc *skillCfgControl) SkillList() []tui.SkillConfigInfo {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := make([]tui.SkillConfigInfo, 0, len(sc.kinds)+len(wizardLocalKinds))
	// Wizard-only skills (wiki/sql/imessage/os) aren't part of the /configure
	// framework — surface them as Legacy so operators can still discover them
	// and get redirected to `tenant setup`.
	seenIDs := map[string]bool{}
	for id, k := range sc.kinds {
		out = append(out, tui.SkillConfigInfo{
			ID:         id,
			Label:      k.Label,
			Configured: sc.isConfigured(id, k),
			Enabled:    sc.isEnabled(id),
			Legacy:     false,
			SetupHint:  k.SetupHint,
		})
		seenIDs[id] = true
	}
	for id, k := range wizardLocalKinds {
		if seenIDs[id] {
			continue
		}
		out = append(out, tui.SkillConfigInfo{
			ID:     id,
			Label:  k.Label,
			Legacy: true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SkillShow renders one skill's field schema with current values.
// Secrets are masked first/last 3 chars when len > 6 (else "***").
// Unknown id → error.
func (sc *skillCfgControl) SkillShow(id string) (string, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	k, ok := sc.kinds[id]
	if !ok {
		// Wizard-only skill — discoverable but configured via `tenant setup`.
		if wk, isLocal := wizardLocalKinds[id]; isLocal {
			return fmt.Sprintf("%s — %s\n  (wizard-only — configure via `tenant setup`)", id, wk.Label), nil
		}
		return "", fmt.Errorf("no skill named %q (use /skill list to see available skills)", id)
	}
	lc, err := loadLaunchConfig(sc.cfgDir)
	if err != nil {
		return "", err
	}
	creds, _ := loadCredentials(sc.cfgDir)
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", id, k.Label)
	fmt.Fprintf(&b, "  state: %s\n", sc.stateLabel(id, k, lc))
	if k.SetupHint != "" {
		fmt.Fprintf(&b, "  hint:  %s\n", k.SetupHint)
	}
	b.WriteString("  fields:\n")
	for _, f := range k.Fields {
		val := sc.fieldValue(id, f, lc, creds)
		display := val
		if f.Secret && val != "" {
			display = maskSecret(val)
		}
		if val == "" {
			display = "(not set)"
		}
		req := ""
		if f.Required {
			req = " (required)"
		} else {
			req = " (optional)"
		}
		secret := ""
		if f.Secret {
			secret = " secret"
		}
		fmt.Fprintf(&b, "    • %s%s%s — %s\n", f.Key, secret, req, display)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// SkillConfigure runs the four-phase pipeline (audit P0):
//  1. parse + validate ALL fields (return composite error on any failure)
//  2. mutate in-memory creds + lc.Skills
//  3. write credentials.json (FIRST — strand a secret > strand an
//     enabled-no-key skill)
//  4. write config.json
//  5. probe + auto-enable (probe failure WARN, doesn't roll back writes)
//
// `args` is the raw arg list after "/skill configure <id>". Supports:
//   - 1 positional value (only when the kind has exactly 1 field)
//   - N "key=value" pairs
//   - Mixing positional + kv is rejected with a pointed error.
//
// `noEnable=true` suppresses the auto-enable step on probe success.
func (sc *skillCfgControl) SkillConfigure(args []string, noEnable bool) (string, error) {
	if len(args) == 0 {
		return "", errors.New("usage: /skill configure <id> <value>   |   /skill configure <id> key=value [key=value …]")
	}
	id := args[0]
	rest := args[1:]

	sc.mu.Lock()
	defer sc.mu.Unlock()

	k, ok := sc.kinds[id]
	if !ok {
		if _, isLocal := wizardLocalKinds[id]; isLocal {
			return "", fmt.Errorf("%q is a wizard-only skill — configure it via `tenant setup`", id)
		}
		return "", fmt.Errorf("no skill named %q (use /skill list to see available skills)", id)
	}
	if !k.Wired {
		return "", fmt.Errorf("%s is in the catalog but its backend is not yet implemented", k.Label)
	}

	// --- Phase 1: parse args into a field-name → value map.
	values, err := parseConfigureArgs(k, rest)
	if err != nil {
		return "", err
	}

	// --- Phase 1b: validate every field (atomic — all-or-nothing).
	// Apply Default when the operator omitted a field AND no pre-existing
	// value is on disk. Skip ShowIf-hidden fields entirely (added in the
	// TEN-65 follow-up: gsuite's sa_json/subject are Required ONLY when
	// auth=sa; under auth=gcloud they shouldn't trigger missing-required
	// errors). ShowIf is evaluated against the values map built so far,
	// so field ordering matters — declare conditional fields AFTER the
	// field they branch on.
	var validationErrs []string
	for _, f := range k.Fields {
		// Hidden ⇒ skip validation AND persistence for this field.
		if f.ShowIf != nil && !f.ShowIf(values) {
			continue
		}
		v, given := values[f.Key]
		if !given {
			pre := sc.fieldValueByKey(id, f)
			if pre != "" {
				continue
			}
			if f.Default != "" {
				// Treat the Default as if the operator supplied it —
				// validates AND persists below.
				v = f.Default
				given = true
				values[f.Key] = v
			} else if f.Required {
				validationErrs = append(validationErrs, fmt.Sprintf("missing required field %q (prompt: %s)", f.Key, f.Prompt))
				continue
			} else {
				continue
			}
		}
		if f.Validate != nil {
			if err := f.Validate(v); err != nil {
				validationErrs = append(validationErrs, fmt.Sprintf("field %q: %v", f.Key, err))
			}
		}
	}
	if len(validationErrs) > 0 {
		return "", fmt.Errorf("validation failed:\n  - %s", strings.Join(validationErrs, "\n  - "))
	}

	// --- Phase 2: mutate in-memory state.
	lc, err := loadLaunchConfig(sc.cfgDir)
	if err != nil {
		return "", err
	}
	creds, _ := loadCredentials(sc.cfgDir)
	if lc.Skills == nil {
		lc.Skills = map[string]*skillConfig{}
	}
	sk := lc.Skills[id]
	if sk == nil {
		sk = &skillConfig{Settings: map[string]string{}}
	}
	if sk.Settings == nil {
		sk.Settings = map[string]string{}
	}
	for _, f := range k.Fields {
		v, given := values[f.Key]
		if !given {
			continue
		}
		if f.Secret {
			creds.set(skillSecretID(id, f.Key), v)
		} else {
			sk.Settings[f.Key] = v
		}
	}
	lc.Skills[id] = sk

	// --- Phase 3: write credentials.json FIRST (audit P0).
	if err := creds.save(sc.cfgDir); err != nil {
		return "", fmt.Errorf("save credentials.json: %w", err)
	}

	// --- Phase 4: write config.json.
	if err := lc.save(sc.cfgDir); err != nil {
		return "", fmt.Errorf("save config.json (credentials were written, retry the configure to align): %w", err)
	}

	// --- Phase 5: probe + auto-enable.
	var out strings.Builder
	fmt.Fprintf(&out, "✓ configured %s\n", id)
	if k.Probe == nil {
		// No probe defined; mark configured + optionally enable.
		if !noEnable {
			if msg, err := sc.tryAutoEnable(id); err != nil {
				fmt.Fprintf(&out, "  ! %s\n", err.Error())
			} else if msg != "" {
				fmt.Fprintf(&out, "  ✓ %s\n", msg)
			}
		}
		return strings.TrimRight(out.String(), "\n"), nil
	}

	identity, probeErr := sc.runProbeWithSave(k, creds, sk.Settings)
	if probeErr != nil {
		fmt.Fprintf(&out, "  ! probe FAILED — credentials stored but unverified: %v\n", probeErr)
		fmt.Fprintf(&out, "  → run `/skill probe %s` to retry, or `/skill clear %s <field>` to undo\n", id, id)
		// Do NOT auto-enable on probe failure (audit P0 / TEN-64 contract).
		return strings.TrimRight(out.String(), "\n"), nil
	}
	fmt.Fprintf(&out, "  ✓ probe OK — %s\n", identity)

	// atlassian "mcp" mode is SELF-ENABLING: the probe connected via the remote
	// MCP connector, which already brought its tools live under the mcp:<host>
	// label. The generic auto-enable targets the NATIVE "atlassian" plugin (no
	// site in mcp mode), so it would spuriously report "not configured" — skip it.
	mcpSelfEnabled := id == "atlassian" && sk.Settings["auth"] == "mcp"
	if !noEnable && !mcpSelfEnabled {
		if msg, err := sc.tryAutoEnable(id); err != nil {
			fmt.Fprintf(&out, "  ! %s\n", err.Error())
		} else if msg != "" {
			fmt.Fprintf(&out, "  ✓ %s\n", msg)
		}
	}
	// Hot-rebuild the Discord relay with the new token AND auto-start it
	// bound to the operator's user ID. Only fires when the probe passed
	// (token verified) and the callback is wired. Also persists
	// Relay.Enabled=true + Relay.OperatorID so the relay auto-boots on
	// every future launch. A failure is a warning, not a roll-back — the
	// token + operator_id are already stored on disk and will be used on
	// next launch.
	if id == "discord" && sc.onDiscordConfigured != nil {
		newToken := creds.get(skillSecretID("discord", "token"))
		operatorID := ""
		if lc.Skills["discord"] != nil {
			operatorID = lc.Skills["discord"].Settings["operator_id"]
		}
		if operatorID != "" {
			lc.Relay.OperatorID = operatorID
			lc.Relay.Enabled = true
			if err := lc.save(sc.cfgDir); err != nil {
				fmt.Fprintf(&out, "  ! could not persist relay config: %v\n", err)
			}
		}
		if err := sc.onDiscordConfigured(newToken, operatorID); err != nil {
			fmt.Fprintf(&out, "  ! relay: %v\n", err)
		}
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// SkillFields returns the catalog's field schema for one id. Drives
// the interactive `/configure` walkthrough — the TUI iterates the
// returned list and prompts for each in turn.
func (sc *skillCfgControl) SkillFields(id string) ([]tui.SkillField, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	k, ok := sc.kinds[id]
	if !ok {
		return nil, fmt.Errorf("no skill named %q in the new catalog", id)
	}
	out := make([]tui.SkillField, 0, len(k.Fields))
	for _, f := range k.Fields {
		out = append(out, tui.SkillField{
			Key: f.Key, Prompt: f.Prompt, Secret: f.Secret,
			Required: f.Required, Default: f.Default,
			Options: f.Options, OptionLabels: f.OptionLabels,
			ShowIf: f.ShowIf, NoteAfter: f.NoteAfter,
		})
	}
	return out, nil
}

// SkillProbe runs the probe without changing config. WARN on failure;
// success returns the identity string.
func (sc *skillCfgControl) SkillProbe(id string) (string, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	k, ok := sc.kinds[id]
	if !ok {
		return "", fmt.Errorf("no skill named %q in the new catalog (legacy skills can't be probed yet)", id)
	}
	if k.Probe == nil {
		return fmt.Sprintf("%s has no probe — skipping", id), nil
	}
	lc, err := loadLaunchConfig(sc.cfgDir)
	if err != nil {
		return "", err
	}
	creds, _ := loadCredentials(sc.cfgDir)
	settings := map[string]string{}
	if lc.Skills != nil && lc.Skills[id] != nil && lc.Skills[id].Settings != nil {
		settings = lc.Skills[id].Settings
	}
	identity, err := sc.runProbeWithSave(k, creds, settings)
	if err != nil {
		return fmt.Sprintf("✗ %s probe failed: %v", id, err), nil
	}
	return fmt.Sprintf("✓ %s probe OK — %s", id, identity), nil
}

// SkillClear removes one field's value. If a Required field is cleared
// on an enabled skill, the skill is auto-disabled (audit footnote).
func (sc *skillCfgControl) SkillClear(id, fieldKey string) (string, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	k, ok := sc.kinds[id]
	if !ok {
		return "", fmt.Errorf("no skill named %q in the new catalog", id)
	}
	var field *skillKindField
	for i := range k.Fields {
		if k.Fields[i].Key == fieldKey {
			field = &k.Fields[i]
			break
		}
	}
	if field == nil {
		return "", fmt.Errorf("no field %q on skill %q (use /skill show %s)", fieldKey, id, id)
	}

	lc, err := loadLaunchConfig(sc.cfgDir)
	if err != nil {
		return "", err
	}
	creds, _ := loadCredentials(sc.cfgDir)
	if field.Secret {
		delete(creds.Secrets, skillSecretID(id, fieldKey))
	} else if lc.Skills != nil && lc.Skills[id] != nil && lc.Skills[id].Settings != nil {
		delete(lc.Skills[id].Settings, fieldKey)
	}

	// Auto-disable if a Required field was cleared and skill is enabled.
	disabled := false
	if field.Required && lc.Skills != nil && lc.Skills[id] != nil && lc.Skills[id].Enabled {
		lc.Skills[id].Enabled = false
		disabled = true
	}

	// Write order matches Configure: creds first, then config.
	if err := creds.save(sc.cfgDir); err != nil {
		return "", fmt.Errorf("save credentials.json: %w", err)
	}
	if err := lc.save(sc.cfgDir); err != nil {
		return "", fmt.Errorf("save config.json (credentials were updated, retry the clear to align): %w", err)
	}

	// Disable on the tool mux too (so the slash agent state matches disk).
	if disabled && sc.setPluginEnabled != nil {
		_, _, _ = sc.setPluginEnabled(id, false)
	}

	msg := fmt.Sprintf("cleared %s.%s", id, fieldKey)
	if disabled {
		msg += " (skill disabled — required field)"
	}
	return msg, nil
}

// --- helpers --------------------------------------------------------

// runProbe wraps the platform's Probe with a context timeout and a
// goroutine-based escape hatch (audit P1): a probe that ignores
// ctx.Done() leaks a goroutine but cannot block the caller past the
// timeout. Timeout is k.ProbeTimeout (default 5s); gsuite's oauth
// path bumps this to 5 min because the operator has to interact with
// a browser (TEN-71). The save callback persists in-flight credential
// updates from the probe (TEN-71: oauth caches the new refresh
// token mid-probe so a crash between mint and save doesn't strand
// the operator).
func (sc *skillCfgControl) runProbeWithSave(k skillKind, creds *credentials, settings map[string]string) (string, error) {
	if k.Probe == nil {
		return "", errors.New("no probe defined")
	}
	timeout := k.ProbeTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	save := func() error { return creds.save(sc.cfgDir) }
	done := make(chan struct {
		identity string
		err      error
	}, 1)
	go func() {
		id, err := k.Probe(ctx, creds, settings, save)
		done <- struct {
			identity string
			err      error
		}{id, err}
	}()
	select {
	case r := <-done:
		return r.identity, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("probe timed out after %v (the probe function did not honor ctx.Done — file a bug for skill %q)", timeout, k.ID)
	}
}

// runProbe is the old free-function entry point, preserved for tests
// that constructed an ad-hoc probe without a full skillCfgControl.
// Forwards to runProbeWithSave with a no-op save (audit P0: tests
// shouldn't have to thread a fake credentials store).
func runProbe(k skillKind, creds *credentials, settings map[string]string) (string, error) {
	if k.Probe == nil {
		return "", errors.New("no probe defined")
	}
	timeout := k.ProbeTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct {
		identity string
		err      error
	}, 1)
	go func() {
		id, err := k.Probe(ctx, creds, settings, func() error { return nil })
		done <- struct {
			identity string
			err      error
		}{id, err}
	}()
	select {
	case r := <-done:
		return r.identity, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("probe timed out after %v (the probe function did not honor ctx.Done — file a bug for skill %q)", timeout, k.ID)
	}
}

// tryAutoEnable bridges to the tool mux. Returns a status string on
// success, or an error to surface as a warning. n==0 means the plugin
// isn't wired into this build — distinct surface from real errors
// (audit P0).
func (sc *skillCfgControl) tryAutoEnable(id string) (string, error) {
	if sc.setPluginEnabled == nil {
		return "", nil // unit-test path; no mux to enable against
	}
	n, _, err := sc.setPluginEnabled(id, true)
	if err != nil {
		return "", fmt.Errorf("auto-enable failed: %v", err)
	}
	if n == 0 {
		// Audit P0: skill configured + probed OK but no plugin tools
		// shipped in this build. Common cause: imessage on a non-mac
		// build, or a platform behind a build tag.
		return "", fmt.Errorf("no %q plugin wired into this build — config saved, but no tools are available to enable", id)
	}
	return fmt.Sprintf("enabled %d tool(s) in skill %q", n, id), nil
}

// parseConfigureArgs handles three shapes:
//   - 1 arg, no '=' : positional (only valid for single-field kinds)
//   - N args, all 'k=v' : key=value
//   - mixed : rejected with a pointed error
func parseConfigureArgs(k skillKind, args []string) (map[string]string, error) {
	if len(args) == 0 {
		return map[string]string{}, nil
	}
	hasKV := false
	hasPositional := false
	for _, a := range args {
		if strings.Contains(a, "=") {
			hasKV = true
		} else {
			hasPositional = true
		}
	}
	if hasKV && hasPositional {
		return nil, errors.New("mix of positional and key=value args is not supported — use one form")
	}
	if hasPositional {
		if len(args) != 1 {
			return nil, fmt.Errorf("positional form takes exactly one value; got %d args", len(args))
		}
		if len(k.Fields) != 1 {
			fieldKeys := make([]string, len(k.Fields))
			for i, f := range k.Fields {
				fieldKeys[i] = f.Key
			}
			return nil, fmt.Errorf("skill %q has %d fields (%s) — use key=value form", k.ID, len(k.Fields), strings.Join(fieldKeys, ", "))
		}
		return map[string]string{k.Fields[0].Key: args[0]}, nil
	}
	out := map[string]string{}
	knownFields := map[string]bool{}
	for _, f := range k.Fields {
		knownFields[f.Key] = true
	}
	for _, a := range args {
		i := strings.IndexByte(a, '=')
		key := strings.TrimSpace(a[:i])
		val := strings.TrimSpace(a[i+1:])
		if !knownFields[key] {
			expected := make([]string, len(k.Fields))
			for i, f := range k.Fields {
				expected[i] = f.Key
			}
			return nil, fmt.Errorf("unknown field %q for skill %q — expected: %s", key, k.ID, strings.Join(expected, ", "))
		}
		out[key] = val
	}
	return out, nil
}

// maskSecret renders a secret as the first 3 and last 3 chars joined
// by ellipsis. Strings under 7 chars become "***" (any prefix/suffix
// would leak too much of a short token).
func maskSecret(s string) string {
	if len(s) < 7 {
		return "***"
	}
	return s[:3] + "…" + s[len(s)-3:]
}

func (sc *skillCfgControl) isConfigured(id string, k skillKind) bool {
	lc, err := loadLaunchConfig(sc.cfgDir)
	if err != nil {
		return false
	}
	creds, _ := loadCredentials(sc.cfgDir)
	for _, f := range k.Fields {
		if !f.Required {
			continue
		}
		v := ""
		if f.Secret {
			v = creds.get(skillSecretID(id, f.Key))
		} else if lc.Skills != nil && lc.Skills[id] != nil && lc.Skills[id].Settings != nil {
			v = lc.Skills[id].Settings[f.Key]
		}
		if v == "" {
			return false
		}
	}
	return true
}

func (sc *skillCfgControl) isEnabled(id string) bool {
	lc, err := loadLaunchConfig(sc.cfgDir)
	if err != nil || lc.Skills == nil || lc.Skills[id] == nil {
		return false
	}
	return lc.Skills[id].Enabled
}

func (sc *skillCfgControl) stateLabel(id string, k skillKind, lc *launchConfig) string {
	configured := sc.isConfigured(id, k)
	enabled := lc.Skills != nil && lc.Skills[id] != nil && lc.Skills[id].Enabled
	switch {
	case !configured:
		return "unconfigured"
	case configured && !enabled:
		return "configured, disabled"
	case configured && enabled:
		return "configured, enabled"
	}
	return "unknown"
}

func (sc *skillCfgControl) fieldValue(id string, f skillKindField, lc *launchConfig, creds *credentials) string {
	if f.Secret {
		return creds.get(skillSecretID(id, f.Key))
	}
	if lc.Skills == nil || lc.Skills[id] == nil || lc.Skills[id].Settings == nil {
		return ""
	}
	return lc.Skills[id].Settings[f.Key]
}

// fieldValueByKey reads from disk for one field — used in Configure's
// validation phase to see if a Required field is already satisfied.
func (sc *skillCfgControl) fieldValueByKey(id string, f skillKindField) string {
	lc, err := loadLaunchConfig(sc.cfgDir)
	if err != nil {
		return ""
	}
	creds, _ := loadCredentials(sc.cfgDir)
	return sc.fieldValue(id, f, lc, creds)
}

// --- generic validators (reusable by per-platform tickets) ----------

// validateNonEmpty returns a validator that rejects whitespace-only or
// empty strings, naming the field in the error message.
func validateNonEmpty(name string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
}

// validateOneOf returns a validator that rejects any value not in the
// allowed set. Case-sensitive — platforms are responsible for
// canonicalizing (e.g., lowercase auth modes).
func validateOneOf(allowed ...string) func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		for _, a := range allowed {
			if s == a {
				return nil
			}
		}
		return fmt.Errorf("must be one of: %s (got %q)", strings.Join(allowed, ", "), s)
	}
}
