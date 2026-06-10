package main

import "testing"

func TestKeyCatalogLookup(t *testing.T) {
	if s, ok := lookupKeySpec("zai"); !ok || s.Kind != keyProvider {
		t.Errorf("zai should be a provider: %+v ok=%v", s, ok)
	}
	if s, ok := lookupKeySpec("tavily"); !ok || s.Kind != keyDirect {
		t.Errorf("tavily should be direct: %+v ok=%v", s, ok)
	}
	if _, ok := lookupKeySpec("skill:discord:token"); !ok {
		t.Error("discord token should be in the catalog")
	}
	// Excluded / hostile ids must not be mutable.
	for _, bad := range []string{"bogus", "../etc/passwd", "skill:x:bearer", "skill:imessage:password"} {
		if _, ok := lookupKeySpec(bad); ok {
			t.Errorf("%q must NOT be in the mutable catalog", bad)
		}
	}
}

func TestDashKeys_DirectSetRemove(t *testing.T) {
	dir := t.TempDir()
	k := dashKeys{cfgDir: dir}
	if err := k.SetSecret("tavily", "tav-key"); err != nil {
		t.Fatal(err)
	}
	if creds, _ := loadCredentials(dir); creds.get("tavily") != "tav-key" {
		t.Fatalf("tavily not stored: %q", creds.get("tavily"))
	}
	set := false
	for _, v := range k.List() {
		if v.CredID == "tavily" {
			set = v.Set
		}
	}
	if !set {
		t.Error("List should show tavily Set=true")
	}
	if err := k.RemoveSecret("tavily"); err != nil {
		t.Fatal(err)
	}
	if creds, _ := loadCredentials(dir); creds.get("tavily") != "" {
		t.Errorf("tavily should be removed, got %q", creds.get("tavily"))
	}
}

func TestDashKeys_SkillDirect(t *testing.T) {
	dir := t.TempDir()
	k := dashKeys{cfgDir: dir}
	if err := k.SetSecret("skill:discord:token", "bot-token"); err != nil {
		t.Fatal(err)
	}
	if creds, _ := loadCredentials(dir); creds.get("skill:discord:token") != "bot-token" {
		t.Errorf("discord token not stored under the skill id: %q", creds.get("skill:discord:token"))
	}
}

func TestDashKeys_UnknownRejected(t *testing.T) {
	k := dashKeys{cfgDir: t.TempDir()}
	if err := k.SetSecret("bogus", "x"); err == nil {
		t.Error("unknown set should error")
	}
	if err := k.RemoveSecret("../etc"); err == nil {
		t.Error("unknown remove should error")
	}
}

// Provider keys round-trip through AddCloudModel (set) and forget+clearStored
// (remove): the key is stored, the provider registered Stored:true, then on
// remove the key is gone and Auth.Stored is cleared (so resolveSecret won't
// keep returning "" for a Stored provider with no secret).
func TestDashKeys_ProviderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	k := dashKeys{cfgDir: dir, mc: &modelControl{cfgDir: dir}}
	if err := k.SetSecret("zai", "zai-key"); err != nil {
		t.Fatalf("provider set: %v", err)
	}
	if creds, _ := loadCredentials(dir); creds.get("zai") != "zai-key" {
		t.Fatalf("zai key not stored: %q", creds.get("zai"))
	}
	lc, _ := loadLaunchConfig(dir)
	if lc.Providers["zai"] == nil || !lc.Providers["zai"].Auth.Stored {
		t.Fatalf("zai provider not registered with Stored=true: %+v", lc.Providers["zai"])
	}

	if err := k.RemoveSecret("zai"); err != nil {
		t.Fatal(err)
	}
	if creds, _ := loadCredentials(dir); creds.get("zai") != "" {
		t.Errorf("zai key should be gone: %q", creds.get("zai"))
	}
	lc2, _ := loadLaunchConfig(dir)
	if pc := lc2.Providers["zai"]; pc != nil && pc.Auth.Stored {
		t.Error("zai Auth.Stored should be false after remove")
	}
}

// ReloadKeys is a no-op (no error) when no provider is active — e.g. a fresh
// install or a degraded launch where config names no active provider.
func TestReloadKeysNoActiveProvider(t *testing.T) {
	mc := &modelControl{cfgDir: t.TempDir()}
	status, err := mc.ReloadKeys()
	if err != nil || status != "" {
		t.Errorf("no active provider → (\"\", nil); got (%q, %v)", status, err)
	}
}

// maybeReloadProvider must NOT touch the (nil) live agent when the changed key
// isn't the active provider — no reload, no panic.
func TestMaybeReloadProvider_NonActiveNoop(t *testing.T) {
	k := dashKeys{cfgDir: t.TempDir(), mc: &modelControl{cfgDir: t.TempDir()}}
	k.maybeReloadProvider("zai") // no active provider configured → returns without reloading
}

func TestCredsModTime(t *testing.T) {
	if credsModTime("/no/such/file") != 0 {
		t.Error("missing file should report mod time 0")
	}
	dir := t.TempDir()
	if err := (dashKeys{cfgDir: dir}).SetSecret("tavily", "k"); err != nil {
		t.Fatal(err)
	}
	if credsModTime(credentialsPath(dir)) == 0 {
		t.Error("existing credentials.json should report a non-zero mod time")
	}
}

// tuiKeys adapts dashKeys for the /configure picker: List maps fields, Set
// delegates to the same persisted path.
func TestTuiKeysAdapter(t *testing.T) {
	dir := t.TempDir()
	k := tuiKeys{dk: dashKeys{cfgDir: dir}}
	if len(k.List()) == 0 {
		t.Fatal("tuiKeys.List should expose the catalog")
	}
	if err := k.Set("tavily", "tk"); err != nil {
		t.Fatal(err)
	}
	if creds, _ := loadCredentials(dir); creds.get("tavily") != "tk" {
		t.Errorf("Set didn't persist: %q", creds.get("tavily"))
	}
	found := false
	for _, it := range k.List() {
		if it.CredID == "tavily" && it.Set {
			found = true
		}
	}
	if !found {
		t.Error("List should reflect the set key")
	}
}

func TestDashKeys_ListEnvDetected(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "from-env")
	k := dashKeys{cfgDir: t.TempDir()}
	for _, v := range k.List() {
		if v.CredID == "tavily" && (!v.EnvDetected || v.EnvVar != "TAVILY_API_KEY") {
			t.Errorf("tavily env not detected: %+v", v)
		}
	}
}
