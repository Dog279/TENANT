package main

// TEN-64: framework tests for the `/skill` integration-config surface.
// All tests use a FAKE catalog injected via newSkillCfgControl so the
// production skillKinds map stays empty and test isolation is clean
// (audit P2).

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeKind builds an in-test skillKind. probeErr lets a test force a
// probe failure; identity is what a successful probe returns.
func fakeKind(probeErr error, identity string) skillKind {
	return skillKind{
		ID: "fake", Label: "Fake (test fixture)", Wired: true,
		Fields: []skillKindField{
			{Key: "token", Prompt: "fake token", Secret: true, Required: true,
				Validate: func(s string) error {
					if len(strings.TrimSpace(s)) < 5 {
						return errors.New("too short (need 5+ chars)")
					}
					return nil
				}},
		},
		Probe: func(ctx context.Context, creds *credentials, settings map[string]string, _ func() error) (string, error) {
			if probeErr != nil {
				return "", probeErr
			}
			return identity, nil
		},
	}
}

// fakeKindMultiField returns a kind with one secret + one non-secret
// field; some tests need to exercise the multi-field path.
func fakeKindMultiField() skillKind {
	return skillKind{
		ID: "multi", Label: "Multi-field fixture", Wired: true,
		Fields: []skillKindField{
			{Key: "url", Prompt: "server url", Required: true,
				Validate: validateNonEmpty("url")},
			{Key: "pass", Prompt: "password", Secret: true, Required: true,
				Validate: validateNonEmpty("password")},
		},
		Probe: func(ctx context.Context, c *credentials, s map[string]string, _ func() error) (string, error) {
			return s["url"], nil
		},
	}
}

// fakeKindOptional returns a kind with a single Required: false field
// (mirrors TEN-69's brave_key shape). Probe always succeeds.
func fakeKindOptional() skillKind {
	return skillKind{
		ID: "opt", Label: "Optional-field fixture", Wired: true,
		Fields: []skillKindField{
			{Key: "k", Prompt: "optional key", Secret: true, Required: false,
				Validate: func(s string) error { return nil }},
		},
		Probe: func(ctx context.Context, c *credentials, s map[string]string, _ func() error) (string, error) {
			return "ok", nil
		},
	}
}

// newCounting* helpers build a fake setPluginEnabled bridge that
// records calls so tests can assert auto-enable behavior.
func newCountingEnabler(returnN int) (counter *int32, fn func(string, bool) (int, string, error)) {
	var c int32
	return &c, func(label string, on bool) (int, string, error) {
		atomic.AddInt32(&c, 1)
		return returnN, "plugin", nil
	}
}

// --- validators ----------------------------------------------------

func TestValidateNonEmpty(t *testing.T) {
	v := validateNonEmpty("token")
	if err := v(""); err == nil {
		t.Error("empty string should fail")
	}
	if err := v("   \t"); err == nil {
		t.Error("whitespace-only should fail")
	}
	if err := v("ok"); err != nil {
		t.Errorf("non-empty should pass; got %v", err)
	}
}

func TestValidateOneOf(t *testing.T) {
	v := validateOneOf("gcloud", "sa")
	if err := v("gcloud"); err != nil {
		t.Errorf("'gcloud' should pass; got %v", err)
	}
	if err := v("sa"); err != nil {
		t.Errorf("'sa' should pass; got %v", err)
	}
	if err := v("nope"); err == nil {
		t.Error("'nope' should fail")
	} else if !strings.Contains(err.Error(), "gcloud, sa") {
		t.Errorf("error should list valid options; got %q", err.Error())
	}
}

// --- maskSecret ----------------------------------------------------

func TestMaskSecret(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "***"},
		{"abc", "***"},
		{"abcdef", "***"},
		{"abcdefg", "abc…efg"},
		{"verylongsecretvaluestring", "ver…ing"},
	}
	for _, c := range cases {
		got := maskSecret(c.in)
		if got != c.want {
			t.Errorf("maskSecret(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// --- parseConfigureArgs --------------------------------------------

func TestParseConfigureArgs_Positional(t *testing.T) {
	k := fakeKind(nil, "id")
	m, err := parseConfigureArgs(k, []string{"abcdef"})
	if err != nil {
		t.Fatalf("positional should succeed; got %v", err)
	}
	if m["token"] != "abcdef" {
		t.Errorf("positional value not stored under field key; got %v", m)
	}
}

func TestParseConfigureArgs_KeyValue(t *testing.T) {
	k := fakeKindMultiField()
	m, err := parseConfigureArgs(k, []string{"url=http://x", "pass=secret"})
	if err != nil {
		t.Fatalf("kv should succeed; got %v", err)
	}
	if m["url"] != "http://x" || m["pass"] != "secret" {
		t.Errorf("kv values wrong: %v", m)
	}
}

func TestParseConfigureArgs_MixedRejected(t *testing.T) {
	k := fakeKindMultiField()
	_, err := parseConfigureArgs(k, []string{"plainval", "url=http://x"})
	if err == nil || !strings.Contains(err.Error(), "positional and key=value") {
		t.Errorf("mixed args should be rejected; got %v", err)
	}
}

func TestParseConfigureArgs_PositionalMultiFieldRejected(t *testing.T) {
	k := fakeKindMultiField()
	_, err := parseConfigureArgs(k, []string{"plainval"})
	if err == nil || !strings.Contains(err.Error(), "key=value") {
		t.Errorf("positional + multi-field should be rejected; got %v", err)
	}
}

func TestParseConfigureArgs_UnknownField(t *testing.T) {
	k := fakeKindMultiField()
	_, err := parseConfigureArgs(k, []string{"unknown=val"})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("unknown field should be rejected; got %v", err)
	}
	if !strings.Contains(err.Error(), "url") || !strings.Contains(err.Error(), "pass") {
		t.Errorf("unknown-field error should list expected fields; got %q", err.Error())
	}
}

// --- SkillList -----------------------------------------------------

func TestSkillList_EmptyCatalog_FallsBackToLegacy(t *testing.T) {
	// Empty new catalog → list should still surface legacy skillSpecs
	// (audit P1: unactionable empty list otherwise).
	sc := newSkillCfgControl(t.TempDir(), nil, nil)
	infos := sc.SkillList()
	if len(infos) == 0 {
		t.Fatal("empty new catalog should fall back to legacy skillSpecs; got 0 entries")
	}
	legacyCount := 0
	for _, i := range infos {
		if i.Legacy {
			legacyCount++
		}
	}
	if legacyCount == 0 {
		t.Errorf("expected at least one Legacy=true entry; got %+v", infos)
	}
}

func TestSkillList_WithFakeCatalog_ListsFake(t *testing.T) {
	sc := newSkillCfgControl(t.TempDir(),
		map[string]skillKind{"fake": fakeKind(nil, "id")}, nil)
	infos := sc.SkillList()
	hasFake := false
	for _, i := range infos {
		if i.ID == "fake" && !i.Legacy {
			hasFake = true
		}
	}
	if !hasFake {
		t.Errorf("fake catalog entry missing from list: %+v", infos)
	}
}

// --- SkillShow -----------------------------------------------------

func TestSkillShow_MasksSecrets(t *testing.T) {
	dir := t.TempDir()
	// Seed a known secret on disk.
	creds, _ := loadCredentials(dir)
	creds.set(skillSecretID("fake", "token"), "supersecretvalue123")
	if err := creds.save(dir); err != nil {
		t.Fatal(err)
	}

	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(nil, "id")}, nil)
	out, err := sc.SkillShow("fake")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "supersecretvalue123") {
		t.Errorf("SkillShow leaked the full secret:\n%s", out)
	}
	if !strings.Contains(out, "sup…123") {
		t.Errorf("SkillShow didn't mask correctly; got:\n%s", out)
	}
}

func TestSkillShow_UnknownReturnsError(t *testing.T) {
	sc := newSkillCfgControl(t.TempDir(),
		map[string]skillKind{"fake": fakeKind(nil, "id")}, nil)
	if _, err := sc.SkillShow("nope"); err == nil {
		t.Error("unknown skill should error")
	}
}

// --- SkillConfigure ------------------------------------------------

func TestSkillConfigure_PositionalSingleField(t *testing.T) {
	dir := t.TempDir()
	counter, enabler := newCountingEnabler(1) // pretend 1 tool was enabled
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(nil, "fake-id")}, enabler)

	out, err := sc.SkillConfigure([]string{"fake", "validtoken"}, false)
	if err != nil {
		t.Fatalf("configure should succeed; got %v", err)
	}
	if !strings.Contains(out, "configured fake") {
		t.Errorf("output missing 'configured fake':\n%s", out)
	}
	if !strings.Contains(out, "probe OK") {
		t.Errorf("output missing probe OK:\n%s", out)
	}
	if !strings.Contains(out, "fake-id") {
		t.Errorf("output should include the probe identity:\n%s", out)
	}
	if atomic.LoadInt32(counter) != 1 {
		t.Errorf("auto-enable should have fired once; got %d", atomic.LoadInt32(counter))
	}
	// Secret should be on disk under the namespaced key.
	creds, _ := loadCredentials(dir)
	if got := creds.get(skillSecretID("fake", "token")); got != "validtoken" {
		t.Errorf("secret not persisted under skill:fake:token; got %q", got)
	}
}

func TestSkillConfigure_ValidationError_NoWrite(t *testing.T) {
	dir := t.TempDir()
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(nil, "id")}, nil)
	_, err := sc.SkillConfigure([]string{"fake", "no"}, false) // "no" < 5 chars
	if err == nil {
		t.Fatal("validation should fail for short token")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("error should reflect the validator message; got %v", err)
	}
	// Nothing should have been written.
	creds, _ := loadCredentials(dir)
	if creds.get(skillSecretID("fake", "token")) != "" {
		t.Error("failed validation must not write the secret (audit P0: atomic write)")
	}
	lc, _ := loadLaunchConfig(dir)
	if lc.Skills["fake"] != nil {
		t.Error("failed validation must not create the skill entry in config")
	}
}

func TestSkillConfigure_ProbeFailure_StoresAndWarns_NoEnable(t *testing.T) {
	dir := t.TempDir()
	counter, enabler := newCountingEnabler(1)
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(errors.New("nope, 401"), "")}, enabler)

	out, err := sc.SkillConfigure([]string{"fake", "validtoken"}, false)
	if err != nil {
		t.Fatalf("configure should not return err on probe failure; got %v", err)
	}
	if !strings.Contains(out, "probe FAILED") || !strings.Contains(out, "401") {
		t.Errorf("output should warn about probe failure with reason:\n%s", out)
	}
	// Credentials must be stored (probe failure is WARN, not rollback).
	creds, _ := loadCredentials(dir)
	if got := creds.get(skillSecretID("fake", "token")); got != "validtoken" {
		t.Error("probe failure should still persist credentials")
	}
	// Auto-enable must NOT fire on probe failure.
	if atomic.LoadInt32(counter) != 0 {
		t.Errorf("auto-enable should be skipped on probe failure; got %d calls", atomic.LoadInt32(counter))
	}
}

func TestSkillConfigure_AutoEnable_NotWiredWarns(t *testing.T) {
	// Audit P0: SetPluginEnabled returning n==0 means the plugin
	// label isn't shipped in this build — configure must surface a
	// distinct warning, not pretend it succeeded.
	dir := t.TempDir()
	_, enabler := newCountingEnabler(0) // 0 tools enabled
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(nil, "id")}, enabler)

	out, err := sc.SkillConfigure([]string{"fake", "validtoken"}, false)
	if err != nil {
		t.Fatalf("configure should succeed (probe ok) even when plugin unwired; got %v", err)
	}
	if !strings.Contains(out, "no \"fake\" plugin wired into this build") {
		t.Errorf("expected unwired-build warning; got:\n%s", out)
	}
}

func TestSkillConfigure_NoEnableFlag_SkipsAutoEnable(t *testing.T) {
	dir := t.TempDir()
	counter, enabler := newCountingEnabler(1)
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(nil, "id")}, enabler)

	if _, err := sc.SkillConfigure([]string{"fake", "validtoken"}, true /* noEnable */); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(counter) != 0 {
		t.Errorf("--no-enable should suppress auto-enable; got %d", atomic.LoadInt32(counter))
	}
}

func TestSkillConfigure_MultiField_KeyValue(t *testing.T) {
	dir := t.TempDir()
	_, enabler := newCountingEnabler(1)
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"multi": fakeKindMultiField()}, enabler)

	if _, err := sc.SkillConfigure(
		[]string{"multi", "url=http://x", "pass=secret123"}, false,
	); err != nil {
		t.Fatalf("multi-field configure should succeed; got %v", err)
	}
	// url stored in config.json settings, pass in credentials.json.
	lc, _ := loadLaunchConfig(dir)
	if lc.Skills["multi"].Settings["url"] != "http://x" {
		t.Errorf("non-secret field not in settings: %v", lc.Skills["multi"].Settings)
	}
	creds, _ := loadCredentials(dir)
	if creds.get(skillSecretID("multi", "pass")) != "secret123" {
		t.Error("secret field not in credentials")
	}
}

// --- SkillProbe (standalone) --------------------------------------

func TestSkillProbe_Success(t *testing.T) {
	dir := t.TempDir()
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(nil, "probe-id")}, nil)
	out, err := sc.SkillProbe("fake")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "probe OK") || !strings.Contains(out, "probe-id") {
		t.Errorf("probe output should include identity:\n%s", out)
	}
}

func TestSkillProbe_FailureReturnsWarn(t *testing.T) {
	dir := t.TempDir()
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(errors.New("revoked"), "")}, nil)
	out, err := sc.SkillProbe("fake")
	if err != nil {
		t.Fatalf("probe failure should not return Go error; got %v", err)
	}
	if !strings.Contains(out, "probe failed") || !strings.Contains(out, "revoked") {
		t.Errorf("probe failure should surface in the output:\n%s", out)
	}
}

func TestSkillProbe_NoProbeDefined(t *testing.T) {
	dir := t.TempDir()
	k := fakeKind(nil, "id")
	k.Probe = nil
	sc := newSkillCfgControl(dir, map[string]skillKind{"fake": k}, nil)
	out, err := sc.SkillProbe("fake")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no probe") {
		t.Errorf("no-probe case should say so: %s", out)
	}
}

// --- runProbe timeout (audit P1: probes that ignore ctx) ----------

func TestRunProbe_HonorsTimeout_OnIgnoredCtx(t *testing.T) {
	// A probe that ignores ctx.Done() must NOT block past ~5s.
	k := skillKind{
		ID: "leaky",
		Probe: func(ctx context.Context, c *credentials, s map[string]string, _ func() error) (string, error) {
			// Deliberately ignore ctx — simulate a buggy probe.
			time.Sleep(8 * time.Second)
			return "should never see this", nil
		},
	}
	start := time.Now()
	_, err := runProbe(k, &credentials{}, map[string]string{})
	elapsed := time.Since(start)
	if elapsed > 6*time.Second {
		t.Errorf("runProbe blocked for %v — must short-circuit on ctx timeout", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error; got %v", err)
	}
}

// --- SkillClear ----------------------------------------------------

func TestSkillClear_RequiredField_DisablesSkill(t *testing.T) {
	dir := t.TempDir()
	_, enabler := newCountingEnabler(1)
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(nil, "id")}, enabler)

	if _, err := sc.SkillConfigure([]string{"fake", "validtoken"}, false); err != nil {
		t.Fatal(err)
	}
	// After configure, skill should be enabled in config (enabler returned 1).
	// Actually — enabler doesn't write to disk. We need to verify via the disk
	// state, which only reflects what skillConfigure wrote. SetPluginEnabled
	// is the runtime side effect; the config-level Enabled flag isn't set
	// by Configure (intentional — `/enable` does that). For this test,
	// pre-set Enabled=true to simulate the live state.
	lc, _ := loadLaunchConfig(dir)
	lc.Skills["fake"].Enabled = true
	_ = lc.save(dir)

	out, err := sc.SkillClear("fake", "token")
	if err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if !strings.Contains(out, "skill disabled") {
		t.Errorf("clearing required field should disable skill:\n%s", out)
	}
	lc2, _ := loadLaunchConfig(dir)
	if lc2.Skills["fake"].Enabled {
		t.Error("Enabled flag should be false after clearing required field")
	}
}

func TestSkillClear_OptionalField_KeepsEnabled(t *testing.T) {
	dir := t.TempDir()
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"opt": fakeKindOptional()}, nil)
	if _, err := sc.SkillConfigure([]string{"opt", "somekey"}, false); err != nil {
		t.Fatal(err)
	}
	lc, _ := loadLaunchConfig(dir)
	if lc.Skills["opt"] == nil {
		lc.Skills = map[string]*skillConfig{"opt": {Settings: map[string]string{}}}
	}
	lc.Skills["opt"].Enabled = true
	_ = lc.save(dir)

	out, err := sc.SkillClear("opt", "k")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "skill disabled") {
		t.Errorf("clearing optional field must NOT disable skill:\n%s", out)
	}
	lc2, _ := loadLaunchConfig(dir)
	if !lc2.Skills["opt"].Enabled {
		t.Error("Enabled should stay true after clearing optional field")
	}
}

func TestSkillClear_UnknownField(t *testing.T) {
	sc := newSkillCfgControl(t.TempDir(),
		map[string]skillKind{"fake": fakeKind(nil, "id")}, nil)
	_, err := sc.SkillClear("fake", "nosuchfield")
	if err == nil || !strings.Contains(err.Error(), "no field") {
		t.Errorf("unknown field should error; got %v", err)
	}
}

// --- production catalog (TEN-65 onward: drift guard, not emptiness) -

// TestSkillKindsLegacyDriftGuard enforces the audit P1 contract: every
// skill in BOTH the new skillKinds catalog AND legacy skillSpecs must
// declare identical field schemas (Secret flag matters; key names
// must match). Prevents the framework + setup wizard from writing
// different namespaces under the same skill id during the TEN-65 →
// TEN-70 migration window.
func TestSkillKindsLegacyDriftGuard(t *testing.T) {
	legacy := map[string]skillSpec{}
	for _, sp := range skillSpecs {
		legacy[sp.ID] = sp
	}
	for id, k := range skillKinds {
		lp, both := legacy[id]
		if !both {
			continue // new-only entries are fine; drift assertion only when both exist
		}
		legacyKeys := map[string]bool{}
		legacySecret := map[string]bool{}
		for _, f := range lp.Fields {
			legacyKeys[f.Key] = true
			legacySecret[f.Key] = f.Secret
		}
		newKeys := map[string]bool{}
		newSecret := map[string]bool{}
		for _, f := range k.Fields {
			newKeys[f.Key] = true
			newSecret[f.Key] = f.Secret
		}
		for key := range legacyKeys {
			if !newKeys[key] {
				t.Errorf("skill %q: legacy has field %q but new catalog does not", id, key)
			} else if legacySecret[key] != newSecret[key] {
				t.Errorf("skill %q field %q: legacy Secret=%v vs new Secret=%v — namespaces will diverge",
					id, key, legacySecret[key], newSecret[key])
			}
		}
		for key := range newKeys {
			if !legacyKeys[key] {
				t.Errorf("skill %q: new catalog has field %q but legacy does not — `tenant setup` won't prompt for it",
					id, key)
			}
		}
	}
}

// --- credentials file location sanity -----------------------------

func TestSkillCfgControl_WritesToExpectedCfgDir(t *testing.T) {
	dir := t.TempDir()
	sc := newSkillCfgControl(dir,
		map[string]skillKind{"fake": fakeKind(nil, "id")}, nil)
	if _, err := sc.SkillConfigure([]string{"fake", "validtoken"}, false); err != nil {
		t.Fatal(err)
	}
	// credentials.json should exist in cfgDir.
	credPath := filepath.Join(dir, "credentials.json")
	if _, err := loadCredentials(dir); err != nil {
		t.Fatalf("credentials.json not loadable: %v (path %s)", err, credPath)
	}
}
