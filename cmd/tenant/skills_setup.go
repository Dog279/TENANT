package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// TEN-70: the setup wizard and the TUI `/skill configure` walkthrough now read
// from ONE catalog. The framework skills (gsuite/atlassian/discord/web/x) live
// in `skillKinds` (skillctl.go) — their fields, validators, notes, and probes
// are defined exactly once there, so there is no second copy to drift from (the
// old `skillSpecs`/`skillField` drift-guard is gone). The skills below are the
// few the wizard configures that are NOT part of the /configure framework
// (path/flag-driven, no API-key probe); they're kept in a SEPARATE map so they
// stay wizard-only and don't leak into the TUI `/configure` picker or the web
// Integrations page (both of which enumerate `skillKinds`).
var wizardLocalKinds = map[string]skillKind{
	"wiki": {ID: "wiki", Label: "Wiki (markdown knowledge base)", Wired: true, Fields: []skillKindField{
		{Key: "dir", Prompt: "Wiki markdown directory"},
	}},
	"sql": {ID: "sql", Label: "SQL (SQLite database)", Wired: true, Fields: []skillKindField{
		{Key: "db", Prompt: "SQLite database file path"},
	}},
	"imessage": {ID: "imessage", Label: "iMessage (via BlueBubbles)", Wired: true, Fields: []skillKindField{
		{Key: "url", Prompt: "BlueBubbles server URL"},
		{Key: "password", Prompt: "BlueBubbles password", Secret: true},
	}},
	"os": {ID: "os", Label: "OS access (sysinfo, files, gated shell)", Wired: true},
}

// wizardOrder is the display order of the setup wizard, preserved from the
// original skillSpecs list. Each id resolves to a skillKinds entry (framework)
// or a wizardLocalKinds entry — wizardCatalog merges them in this order.
var wizardOrder = []string{"wiki", "sql", "gsuite", "x", "imessage", "discord", "web", "os"}

// wizardCatalog returns the wizard's skills as a single ordered []skillKind,
// sourcing each id from skillKinds (framework) or wizardLocalKinds (local).
// Unknown ids (e.g. a skillKinds entry not yet wired) are skipped.
//
// gsuite is adapted for cfgDir exactly as newSkillCfgControl adapts the TUI
// copy (adaptGSuiteForCfgDir) — the global skillKinds["gsuite"] is the raw,
// cfgDir-less entry, so without this the wizard's oauth branch would always
// take the slow-path abort and the opt-in probe would lack the embedded-creds
// reader. Applying it here keeps the wizard and /configure behaviourally
// identical, which is the whole point of TEN-70.
func wizardCatalog(cfgDir string) []skillKind {
	out := make([]skillKind, 0, len(wizardOrder))
	for _, id := range wizardOrder {
		if k, ok := skillKinds[id]; ok {
			if id == "gsuite" {
				k = adaptGSuiteForCfgDir(k, cfgDir)
			}
			out = append(out, k)
			continue
		}
		if k, ok := wizardLocalKinds[id]; ok {
			out = append(out, k)
		}
	}
	return out
}

// maxWizardFieldAttempts caps validation re-asks so a non-interactive /
// EOF-terminated input stream can't spin forever (ask returns the default on
// EOF, which would otherwise loop on a Required+invalid field).
const maxWizardFieldAttempts = 3

// configureSkills runs the interactive skills step: for each integration, ask
// whether to enable it and capture its settings/secrets. It iterates the shared
// catalog (wizardCatalog) so validators, ShowIf gating, NoteAfter hooks, and
// SetupHints all come from the same definitions the TUI uses.
func configureSkills(in *bufio.Reader, cur map[string]*skillConfig, creds *credentials, cfgDir string) map[string]*skillConfig {
	if cur == nil {
		cur = map[string]*skillConfig{}
	}
	fmt.Fprintln(os.Stderr, "\nSkills / integrations (Enter to skip each):")
	for _, k := range wizardCatalog(cfgDir) {
		existing := cur[k.ID]
		def := "n"
		if existing != nil && existing.Enabled {
			def = "y"
		}
		if !yes(ask(in, fmt.Sprintf("Enable %s? [y/n]", k.Label), def)) {
			if existing != nil {
				existing.Enabled = false
			}
			continue
		}
		sc := existing
		if sc == nil {
			sc = &skillConfig{Settings: map[string]string{}}
		}
		if sc.Settings == nil {
			sc.Settings = map[string]string{}
		}

		// Collect field values into a running map so ShowIf can branch on
		// earlier answers (e.g. gsuite's sa_json/subject are asked only when
		// auth=sa). Seed it with the already-saved non-secret settings.
		// Newly-entered secrets are buffered in pendingSecrets and flushed to
		// creds ONLY after the whole skill collects without aborting — so a
		// hard-blocking NoteAfter can never strand a secret in credentials.json
		// (secret-safe by construction, not by field ordering).
		values := map[string]string{}
		for key, v := range sc.Settings {
			values[key] = v
		}
		pendingSecrets := map[string]string{}
		aborted := false
		for _, f := range k.Fields {
			if f.ShowIf != nil && !f.ShowIf(values) {
				continue
			}
			v := askSkillField(in, k.ID, f, creds, sc.Settings, pendingSecrets)
			if v != "" {
				values[f.Key] = v
			}
			if f.NoteAfter != nil {
				msg, abort := f.NoteAfter(v)
				if msg != "" {
					fmt.Fprintf(os.Stderr, "    → %s\n", msg)
				}
				if abort {
					aborted = true
					break
				}
			}
		}
		if aborted {
			// Matches the TUI: a hard-blocking NoteAfter stops this skill;
			// don't enable it and discard everything collected (pendingSecrets
			// are never written; non-secret Settings aren't persisted).
			if existing != nil {
				existing.Enabled = false
			}
			continue
		}

		// No abort — commit. Secrets first (audit P0 ordering), then non-secret
		// Settings. Skip ShowIf-hidden fields. ShowIf-hidden settings from a
		// previously-configured branch are intentionally RETAINED (not deleted),
		// matching SkillConfigure (skillctl.go) so the wizard and /configure
		// stay aligned; the plugins read only the fields valid for the active
		// auth mode, so a stale sibling-branch path is inert.
		sc.Enabled = true
		for secretID, v := range pendingSecrets {
			creds.set(secretID, v)
		}
		for _, f := range k.Fields {
			if f.Secret {
				continue
			}
			if f.ShowIf != nil && !f.ShowIf(values) {
				continue
			}
			if v, ok := values[f.Key]; ok {
				sc.Settings[f.Key] = strings.TrimSpace(v)
			}
		}
		if k.SetupHint != "" {
			fmt.Fprintf(os.Stderr, "    → %s\n", k.SetupHint)
		}
		// Optional liveness check — opt-in (default no) so `tenant setup` works
		// offline and never opens an OAuth browser unprompted. WARN, never block.
		if k.Probe != nil && yes(ask(in, fmt.Sprintf("  Verify %s now? [y/N]", k.ID), "n")) {
			if identity, err := runProbe(k, creds, sc.Settings); err != nil {
				fmt.Fprintf(os.Stderr, "    ! probe FAILED — config stored but unverified: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "    ✓ probe OK — %s\n", identity)
			}
		}
		cur[k.ID] = sc
	}
	return cur
}

// askSkillField prompts for one catalog field, honoring secrets (with a
// "(keep saved)" affordance), defaults, an Options hint, and re-asking on a
// Validate failure with the SAME wording the TUI surfaces. It returns the
// resolved value: for a kept secret, the existing stored value; for a skipped
// optional field, "". A newly-entered secret is buffered in pendingSecrets
// (NOT written to creds here) so the caller can discard it if the skill later
// aborts — kept secrets are already on disk and untouched.
func askSkillField(in *bufio.Reader, id string, f skillKindField, creds *credentials, settings, pendingSecrets map[string]string) string {
	prompt := "  " + f.Prompt
	if len(f.Options) > 0 {
		prompt += " (" + strings.Join(f.Options, "|") + ")"
	}
	for attempt := 0; attempt < maxWizardFieldAttempts; attempt++ {
		if f.Secret {
			existing := creds.get(skillSecretID(id, f.Key))
			curDef := ""
			if existing != "" {
				curDef = "(keep saved)"
			}
			raw := ask(in, prompt, curDef)
			if raw == "" || raw == "(keep saved)" {
				return existing // keep what's on disk
			}
			if f.Validate != nil {
				if err := f.Validate(raw); err != nil {
					fmt.Fprintf(os.Stderr, "    ✗ %v\n", err)
					continue
				}
			}
			pendingSecrets[skillSecretID(id, f.Key)] = raw // flushed by caller iff no abort
			return raw
		}
		// Non-secret: default to the saved value, else the catalog Default.
		def := settings[f.Key]
		if def == "" {
			def = f.Default
		}
		raw := strings.TrimSpace(ask(in, prompt, def))
		if raw == "" {
			if f.Required && def == "" {
				fmt.Fprintf(os.Stderr, "    ✗ %s is required\n", f.Key)
				continue
			}
			return def
		}
		if f.Validate != nil {
			if err := f.Validate(raw); err != nil {
				fmt.Fprintf(os.Stderr, "    ✗ %v\n", err)
				continue
			}
		}
		return raw
	}
	fmt.Fprintf(os.Stderr, "    ! skipping %s after %d invalid attempts\n", f.Key, maxWizardFieldAttempts)
	return ""
}

func skillSecretID(skill, field string) string { return "skill:" + skill + ":" + field }

func yes(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "true", "1":
		return true
	}
	return false
}

// applyPluginConfig fills pluginFlags from the saved skills config for any
// flag the operator did NOT pass explicitly — so `tenant tui` honors what
// `tenant setup` configured, without re-typing plugin flags. Explicit flags
// always win (checked via the shared FlagSet's Visit set).
func applyPluginConfig(c *commonFlags, pf *pluginFlags) {
	if c == nil || c.lc == nil || len(c.lc.Skills) == 0 {
		return
	}
	set := c.flagSet()
	get := func(skill, key string) string {
		if s := c.lc.Skills[skill]; s != nil && s.Settings != nil {
			return s.Settings[key]
		}
		return ""
	}
	enabled := func(skill string) bool {
		s := c.lc.Skills[skill]
		return s != nil && s.Enabled
	}
	secret := func(skill, field string) string {
		return resolveSecret(c.cfgDir, skillSecretID(skill, field), authCfg{Stored: true})
	}

	if enabled("wiki") && !set["wiki-dir"] {
		pf.wikiDir = firstNonEmpty(pf.wikiDir, get("wiki", "dir"))
	}
	if enabled("sql") && !set["sql-db"] {
		pf.sqlDB = firstNonEmpty(pf.sqlDB, get("sql", "db"))
	}
	if enabled("gsuite") {
		if !set["gsuite"] {
			pf.gsuite = true
		}
		if !set["gsuite-auth"] && get("gsuite", "auth") != "" {
			pf.gsuiteAuth = get("gsuite", "auth")
		}
		if !set["gsuite-sa-json"] {
			pf.gsuiteSAJSON = firstNonEmpty(pf.gsuiteSAJSON, get("gsuite", "sa_json"))
		}
		if !set["gsuite-subject"] {
			pf.gsuiteSubject = firstNonEmpty(pf.gsuiteSubject, get("gsuite", "subject"))
		}
	}
	if enabled("x") {
		if !set["x"] {
			pf.x = true
		}
		if !set["x-bearer"] {
			pf.xBearer = firstNonEmpty(pf.xBearer, secret("x", "bearer"))
		}
	}
	if enabled("imessage") {
		if !set["imessage"] {
			pf.imsg = true
		}
		if !set["bb-url"] {
			pf.imsgURL = firstNonEmpty(pf.imsgURL, get("imessage", "url"))
		}
		if !set["bb-password"] {
			pf.imsgPass = firstNonEmpty(pf.imsgPass, secret("imessage", "password"))
		}
	}
	if enabled("discord") {
		if !set["discord"] {
			pf.discord = true
		}
		if !set["discord-bot-token"] {
			pf.discordToken = firstNonEmpty(pf.discordToken, secret("discord", "token"))
		}
	}
	if enabled("web") && !set["web"] {
		pf.web = true
	}
	if enabled("os") && !set["os"] {
		pf.osEnable = true
	}
}
