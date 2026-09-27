package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/plugins/discord"
	xp "tenant/internal/plugins/x"
	"tenant/internal/secrets"
	"tenant/internal/tui"
)

// TEN-338: `tenant permissions effective` and `tenant security audit` report
// the authority the runtime grants: launch flags included, every surface
// composed, tool rules and the Discord exec mode applied.

// The ticket's regression: `tui --os --os-allow-exec` seeds exec=allow, so
// `permissions effective --os --os-allow-exec` reported ask while the broker
// allowed host exec.
func TestPermissionsEffectiveHonorsLaunchFlags(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	report := runPermissionsEffectiveJSON(t, "--config", cfgDir, "--data", dataDir, "--os", "--os-allow-exec")
	if exec := report.Categories[catExec]; exec.Mode != "allow" || exec.Source != "launch flag --os-allow-exec" {
		t.Fatalf("exec = %+v, want allow from the launch flag", exec)
	}
	if !report.Execution.Enabled {
		t.Error("execution should be enabled with --os and exec allowed")
	}

	// Saved modes load after the flags, as they do at launch.
	stg := &settings{Permissions: map[string]string{catExec: "ask"}}
	if err := stg.Save(cfgDir, "main"); err != nil {
		t.Fatal(err)
	}
	report = runPermissionsEffectiveJSON(t, "--config", cfgDir, "--data", dataDir, "--os", "--os-allow-exec")
	if exec := report.Categories[catExec]; exec.Mode != "ask" || exec.Source != "settings permissions" {
		t.Fatalf("exec = %+v, want the saved ask to override the flag", exec)
	}
}

// ...and `security audit --os --os-allow-exec` exited zero.
func TestSecurityAuditFailsOnLaunchFlagHostExec(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	report, err := runSecurityAuditJSON(t, "--config", cfgDir, "--data", dataDir, "--os", "--os-allow-exec")
	if err == nil {
		t.Fatal("security audit must exit non-zero when a launch flag allows host exec")
	}
	if !hasSecurityFinding(report.Findings, "permissions.host-exec-allow") {
		t.Fatalf("findings = %+v, want permissions.host-exec-allow", report.Findings)
	}
}

// An enabled relay in exec mode with exec=allow gives a Discord message a
// shell on this host with no one approving; the audit used to look at the
// local surface only and exit zero.
func TestSecurityAuditEvaluatesDiscordSurface(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	lc := &launchConfig{Relay: relayConfig{
		Enabled: true, OperatorID: "op", AllowExec: true,
		Permissions: map[string]string{catExec: "allow", catSend: "allow"},
	}}
	saveConfig := func() {
		t.Helper()
		if err := lc.Save(cfgDir); err != nil {
			t.Fatal(err)
		}
	}
	saveConfig()
	report, err := runSecurityAuditJSON(t, "--config", cfgDir, "--data", dataDir)
	if err == nil {
		t.Fatalf("remote shell without approval must fail the audit; findings=%+v", report.Findings)
	}
	for _, id := range []string{"permissions.discord.exec-allow", "permissions.discord.send-allow"} {
		if !hasSecurityFinding(report.Findings, id) {
			t.Errorf("missing %s in %+v", id, report.Findings)
		}
	}
	if hasSecurityFinding(report.Findings, "permissions.host-exec-allow") {
		t.Error("the local surface is still ask by default")
	}

	// Exec mode off: the broker refuses exec whatever relay.permissions says,
	// and the report says so instead of the allow.
	lc.Relay.AllowExec = false
	saveConfig()
	report, err = runSecurityAuditJSON(t, "--config", cfgDir, "--data", dataDir)
	if err != nil {
		t.Fatalf("exec mode off leaves nothing critical: %v; findings=%+v", err, report.Findings)
	}
	relay := surfaceByName(t, report.Surfaces, permissionSurfaceDiscord)
	if exec := relay.Categories[catExec]; exec.Mode != "deny" || exec.Source != "relay.allow_exec=false" {
		t.Errorf("exec with exec mode off = %+v", exec)
	}
	// Discord's own posts still follow the send mode, so that warning stays.
	if send := relay.Categories[catSend]; send.Mode != "allow" || !strings.Contains(send.Source, "refused by relay.allow_exec=false") {
		t.Errorf("send with exec mode off = %+v", send)
	}
	if !hasSecurityFinding(report.Findings, "permissions.discord.send-allow") {
		t.Errorf("Discord posts without approval should still warn: %+v", report.Findings)
	}

	// A relay that is off is listed, with what it would grant, but raises nothing.
	lc.Relay.Enabled, lc.Relay.AllowExec = false, true
	saveConfig()
	report, err = runSecurityAuditJSON(t, "--config", cfgDir, "--data", dataDir)
	if err != nil {
		t.Fatalf("a disabled relay must not fail the audit: %v", err)
	}
	relay = surfaceByName(t, report.Surfaces, permissionSurfaceDiscord)
	if relay.Active || relay.Categories[catExec].Mode != "allow" {
		t.Errorf("disabled relay = %+v", relay)
	}
	if hasSecurityFinding(report.Findings, "permissions.discord.exec-allow") {
		t.Error("an inactive surface must not raise findings")
	}
}

func TestSecurityAuditEvaluatesIMessageSurface(t *testing.T) {
	c := &commonFlags{agent: "main", cfgDir: t.TempDir(), lc: &launchConfig{IMessage: imessageConfig{
		Enabled: true, AllowFrom: []string{"+15550100"},
		Permissions: map[string]string{catWrite: "allow"},
	}}}
	surfaces, err := composeSurfaceAudits(c, &pluginFlags{}, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	imsg := surfaceByName(t, surfaces, permissionSurfaceIMessage)
	if !imsg.Active {
		t.Fatalf("an enabled responder with an allowlist on macOS is active: %+v", imsg.authoritySurface)
	}
	if w := imsg.Categories[catWrite]; w.Mode != "allow" || w.Source != "imessage.permissions" {
		t.Errorf("write = %+v", w)
	}
	if e := imsg.Categories[catExec]; e.Mode != "deny" || e.Source != "imessage deny-by-default" {
		t.Errorf("exec = %+v", e)
	}
	report := securityAuditReport{Surfaces: surfaces}
	evaluateSecurityAudit(&report)
	if report.Critical != 1 || !hasSecurityFinding(report.Findings, "permissions.imessage.write-allow") {
		t.Fatalf("findings = %+v, want one critical permissions.imessage.write-allow", report.Findings)
	}

	// Off macOS the responder cannot run, so the same config raises nothing.
	surfaces, err = composeSurfaceAudits(c, &pluginFlags{}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	report = securityAuditReport{Surfaces: surfaces}
	evaluateSecurityAudit(&report)
	if report.Critical != 0 {
		t.Errorf("an inactive iMessage surface raised %+v", report.Findings)
	}
}

func TestAuthoritySurfaces(t *testing.T) {
	get := func(lc *launchConfig, goos, surface string) authoritySurface {
		t.Helper()
		for _, s := range authoritySurfaces(lc, goos) {
			if s.Surface == surface {
				return s
			}
		}
		t.Fatalf("no %s surface", surface)
		return authoritySurface{}
	}
	if s := get(nil, "linux", permissionSurfaceLocal); !s.Active {
		t.Error("the local surface is always active")
	}
	if s := get(nil, "linux", permissionSurfaceDiscord); s.Active || s.Reason != "relay.enabled=false" {
		t.Errorf("discord with no config = %+v", s)
	}
	if s := get(&launchConfig{Relay: relayConfig{Enabled: true}}, "linux", permissionSurfaceDiscord); !s.Active {
		t.Errorf("an enabled relay is active: %+v", s)
	}
	on := &launchConfig{IMessage: imessageConfig{Enabled: true}}
	if s := get(on, "windows", permissionSurfaceIMessage); s.Active || !strings.Contains(s.Reason, "macOS only") {
		t.Errorf("imessage off macOS = %+v", s)
	}
	if s := get(on, "darwin", permissionSurfaceIMessage); s.Active || !strings.Contains(s.Reason, "allow_from is empty") {
		t.Errorf("imessage with nobody allowed = %+v", s)
	}
	on.IMessage.AllowFrom = []string{"a@example.com"}
	if s := get(on, "darwin", permissionSurfaceIMessage); !s.Active {
		t.Errorf("imessage on macOS with an allowlist = %+v", s)
	}
}

// Tool rules are checked before the category on every surface, so an allow
// rule for os_exec is host exec without approval wherever a turn comes from,
// except through a Discord exec mode that is off: the lock outranks rules.
func TestComposeFoldsToolRules(t *testing.T) {
	cfgDir := t.TempDir()
	stg := &settings{ToolRules: map[string]string{"os_exec": "allow", "os_write": "deny"}}
	if err := stg.Save(cfgDir, "main"); err != nil {
		t.Fatal(err)
	}
	c := &commonFlags{agent: "main", cfgDir: cfgDir, lc: &launchConfig{}}
	compose := func(pf *pluginFlags, surface string) effectivePermissions {
		t.Helper()
		report, err := composeEffectivePermissions(c, pf, surface)
		if err != nil {
			t.Fatal(err)
		}
		return report
	}

	local := compose(&pluginFlags{}, permissionSurfaceLocal)
	if e := local.Categories[catExec]; e.Mode != "allow" || e.Source != "tool rule os_exec" {
		t.Errorf("local exec = %+v", e)
	}
	// One write action denied by a rule leaves the other three on the category.
	if w := local.Categories[catWrite]; w.Mode != "ask" || w.Source != "local default" {
		t.Errorf("local write = %+v", w)
	}
	if local.ToolRules["os_exec"] != "allow" {
		t.Errorf("the report should list the rules: %+v", local.ToolRules)
	}
	if e := compose(&pluginFlags{}, permissionSurfaceIMessage).Categories[catExec]; e.Mode != "allow" {
		t.Errorf("a rule beats iMessage deny-by-default: %+v", e)
	}
	if e := compose(&pluginFlags{}, permissionSurfaceDiscord).Categories[catExec]; e.Mode != "deny" || e.Source != "relay.allow_exec=false" {
		t.Errorf("discord exec with exec mode off = %+v", e)
	}
	c.lc.Relay.AllowExec = true
	if e := compose(&pluginFlags{}, permissionSurfaceDiscord).Categories[catExec]; e.Mode != "allow" || e.Source != "tool rule os_exec" {
		t.Errorf("discord exec with exec mode on = %+v", e)
	}

	// An ask rule forces a prompt past a category allow.
	stg.ToolRules = map[string]string{"os_exec": "ask"}
	if err := stg.Save(cfgDir, "main"); err != nil {
		t.Fatal(err)
	}
	if e := compose(&pluginFlags{osAllowExec: true}, permissionSurfaceLocal).Categories[catExec]; e.Mode != "ask" || e.Source != "tool rule os_exec" {
		t.Errorf("exec with --os-allow-exec and an ask rule = %+v", e)
	}
}

// The composition and the broker must agree call for call. For each surface,
// with flags, saved modes, rules and the Discord lock in play, build the broker
// the way the runtime does and ask it about every action the category table
// knows (plus an unlisted id for the open categories). The most permissive
// answer in each category must be what the composition reports.
func TestComposeMatchesConfirm(t *testing.T) {
	ruleSet := map[string]string{"os_exec": "ask", "os_append": "allow", "x_post": "deny", "mcp_*": "allow"}
	pf := &pluginFlags{osAllowExec: true, gsuiteAllowSend: true}
	saved := map[string]string{catWrite: "deny", catWeb: "allow", catSandbox: "allow"}
	lc := &launchConfig{
		Relay:    relayConfig{Permissions: map[string]string{catExec: "allow", catSend: "allow", catDestructive: "deny"}},
		IMessage: imessageConfig{Permissions: map[string]string{catSend: "ask", catEgress: "allow"}},
	}
	probes := map[string][]string{}
	for action, cat := range allActionCategories() {
		probes[cat] = append(probes[cat], action)
	}
	probes[catDestructive] = append(probes[catDestructive], "mcp:127.0.0.1:9000:write_note")
	probes[catWeb] = append(probes[catWeb], "web_click")

	for _, allowExec := range []bool{false, true} {
		lc.Relay.AllowExec = allowExec
		for _, surface := range []string{permissionSurfaceLocal, permissionSurfaceDiscord, permissionSurfaceIMessage} {
			cfgDir := t.TempDir()
			stg := &settings{Permissions: saved, ToolRules: ruleSet}
			if err := stg.Save(cfgDir, "main"); err != nil {
				t.Fatal(err)
			}
			report, err := composeEffectivePermissions(&commonFlags{agent: "main", cfgDir: cfgDir, lc: lc}, pf, surface)
			if err != nil {
				t.Fatal(err)
			}

			b := runtimeBroker(surface, pf, stg, lc)
			rules := newToolRules()
			rules.Load(ruleSet)
			b.rules = rules
			if surface == permissionSurfaceDiscord {
				b.locked = func(action, cat string) bool { return !lc.Relay.AllowExec && relayExecLocks(action, cat) }
			}
			asked := false
			b.ask = func(context.Context, tui.ApprovalRequest) tui.ApprovalDecision {
				asked = true
				return tui.DenyOnce
			}
			for cat, actions := range probes {
				best := -1
				for _, action := range actions {
					asked = false
					rank := 0
					if b.Confirm(context.Background(), action, "probe") {
						rank = 2
					} else if asked {
						rank = 1
					}
					best = max(best, rank)
				}
				if got := report.Categories[cat]; modeRank(got.Mode) != best {
					t.Errorf("surface=%s allow_exec=%t %s: composed %+v, Confirm's most permissive answer ranks %d",
						surface, allowExec, cat, got, best)
				}
			}
		}
	}
}

// runtimeBroker builds a surface's broker as cmdTUI and serve do.
func runtimeBroker(surface string, pf *pluginFlags, stg *settings, lc *launchConfig) *approvalBroker {
	switch surface {
	case permissionSurfaceDiscord:
		b := newDiscordApprovalBroker(testLog())
		b.loadModes(lc.Relay.Permissions)
		return b
	case permissionSurfaceIMessage:
		b := newOffsiteApprovalBroker(testLog(), newApprovalBroker(testLog()))
		b.loadModes(lc.IMessage.Permissions)
		return b
	}
	b := newApprovalBroker(testLog())
	b.seedFromFlags(pf)
	b.loadModes(stg.Permissions)
	return b
}

// With exec mode off the Discord broker refuses what the mode keeps closed
// before a tool rule or a category allow is consulted, and keeps Discord's own
// posts on the send mode.
func TestDiscordExecLockOutranksRulesAndModes(t *testing.T) {
	b := newDiscordApprovalBroker(testLog())
	rules := newToolRules()
	rules.Load(map[string]string{"os_exec": "allow"})
	b.rules = rules
	b.loadModes(map[string]string{catSend: "allow", catWrite: "allow"})
	execOn := false
	b.locked = func(action, cat string) bool { return !execOn && relayExecLocks(action, cat) }
	ctx := context.Background()

	if b.Confirm(ctx, "os_exec", "ls") {
		t.Error("an allow rule must not reopen exec while exec mode is off")
	}
	if b.Confirm(ctx, "os_write", "f") {
		t.Error("a write=allow mode must not reopen writes while exec mode is off")
	}
	if b.Confirm(ctx, "gsuite_send", "mail") {
		t.Error("outbound email stays closed while exec mode is off")
	}
	if !b.Confirm(ctx, discord.ActionSendMessage, "hi") {
		t.Error("a Discord post follows the send mode with exec mode off")
	}
	execOn = true
	if !b.Confirm(ctx, "os_exec", "ls") || !b.Confirm(ctx, "gsuite_send", "mail") {
		t.Error("exec mode on hands the calls back to the rules and modes")
	}
}

// The serve wiring gives the Discord broker its exec-mode lock, and restores
// the persisted mode with or without a bot token: the old restore needed one,
// so a token added later started with exec mode off and the next persist wrote
// that back over the operator's choice.
func TestServeRelayWiresExecLock(t *testing.T) {
	for _, allowExec := range []bool{false, true} {
		c := &commonFlags{agent: "main", cfgDir: t.TempDir(), lc: &launchConfig{Relay: relayConfig{AllowExec: allowExec}}}
		m := buildServeRelay(context.Background(), serveRelayDeps{
			c: c, stores: &stores{}, log: testLog(), degraded: &degradedState{},
			emit: func(agent.Event) {}, notify: func(string) {},
		})
		if m.Configured() {
			t.Fatal("no bot token is stored, so no relay agent should be built")
		}
		if m.ExecOn() != allowExec {
			t.Errorf("allow_exec=%t restored as %t without a token", allowExec, m.ExecOn())
		}
		if m.broker.locked == nil {
			t.Fatal("the Discord broker has no exec-mode lock")
		}
		if got := m.broker.locked("os_exec", catExec); got == allowExec {
			t.Errorf("allow_exec=%t: lock on os_exec = %t", allowExec, got)
		}
	}
}

// The file checks cover every managed secret (the peer key and the X token
// cache were missing) and verify protection the way doctor does: a protected
// Windows file is no longer "unverified", and a loose one is a finding.
func TestProtectedFileSpotChecksCoverManagedSecrets(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	cred := credentialsPath(cfgDir)
	if err := secrets.WriteFile(cred, []byte(`{"secrets":{}}`)); err != nil {
		t.Fatal(err)
	}
	loose := xp.TokenPath(filepath.Join(dataDir, "loose"))
	if err := os.MkdirAll(filepath.Dir(loose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	if report := secrets.Probe(loose); report.Protected {
		t.Fatalf("fixture is already protected, so the test proves nothing: %+v", report)
	}

	checks := protectedFileSpotChecks(cfgDir, filepath.Dir(loose), "main", false)
	byPath := map[string]fileProtectionAudit{}
	for _, check := range checks {
		byPath[check.Path] = check
	}
	managed, err := managedSecrets(cfgDir, filepath.Dir(loose))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range managed {
		if _, ok := byPath[secret.path]; !ok {
			t.Errorf("no check for managed secret %s", secret.label)
		}
	}
	if got := byPath[cred]; got.Status != "ok" {
		t.Errorf("protected credentials.json = %+v, want ok", got)
	}
	if got := byPath[loose]; got.Status != "warning" {
		t.Errorf("loose X token cache = %+v, want warning", got)
	}
	report := securityAuditReport{Files: checks}
	evaluateSecurityAudit(&report)
	if !hasSecurityFinding(report.Findings, "file."+filepath.Base(loose)) {
		t.Errorf("findings = %+v, want one for the loose token cache", report.Findings)
	}
}

func runPermissionsEffectiveJSON(t *testing.T, args ...string) effectivePermissions {
	t.Helper()
	out, err := captureTenantStdout(t, func() error {
		return cmdPermissions(context.Background(), append([]string{"effective", "--json"}, args...))
	})
	if err != nil {
		t.Fatalf("permissions effective: %v\n%s", err, out)
	}
	var report effectivePermissions
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode effective JSON: %v\n%s", err, out)
	}
	return report
}

// runSecurityAuditJSON returns the decoded report and the command's error (a
// critical finding is an error).
func runSecurityAuditJSON(t *testing.T, args ...string) (securityAuditReport, error) {
	t.Helper()
	out, err := captureTenantStdout(t, func() error {
		return cmdSecurity(context.Background(), append([]string{"audit", "--json"}, args...))
	})
	var report securityAuditReport
	if decodeErr := json.Unmarshal([]byte(out), &report); decodeErr != nil {
		t.Fatalf("decode audit JSON: %v (command error: %v)\n%s", decodeErr, err, out)
	}
	return report, err
}

func surfaceByName(t *testing.T, surfaces []surfaceAudit, name string) surfaceAudit {
	t.Helper()
	for _, s := range surfaces {
		if s.Surface == name {
			return s
		}
	}
	t.Fatalf("no %s surface in %+v", name, surfaces)
	return surfaceAudit{}
}
