package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"tenant/internal/plugins/onepassword"
)

func opCreds(secrets map[string]string) *credentials {
	return &credentials{Secrets: secrets}
}

// The settings choose how the plugin signs in; an environment variable only
// supplies the credential for the mode chosen.
func TestOnePasswordSignInModeComesFromTheSettings(t *testing.T) {
	for _, k := range []string{"OP_SERVICE_ACCOUNT_TOKEN", "OP_CONNECT_HOST", "OP_CONNECT_TOKEN"} {
		t.Setenv(k, "")
	}
	// A service account is the default, and what a config from before the
	// setting means.
	if _, err := onepasswordBackendFrom(opCreds(nil), map[string]string{}); !errors.Is(err, errNotConfigured) {
		t.Errorf("no token: %v", err)
	}

	// Connect needs its address and its token.
	connect := map[string]string{"auth": "connect", "connect_host": "https://connect.example.test:8443"}
	if _, err := onepasswordBackendFrom(opCreds(nil), connect); !errors.Is(err, errNotConfigured) {
		t.Errorf("connect with no token: %v", err)
	}
	stored := opCreds(map[string]string{"skill:onepassword:connect_token": "eyJstored", "skill:onepassword:token": "ops_unused"})
	be, err := onepasswordBackendFrom(stored, connect)
	c, ok := be.(*onepassword.Connect)
	if err != nil || !ok || c.Host() != "https://connect.example.test:8443" {
		t.Fatalf("connect: %T %v", be, err)
	}

	// A Connect variable in the environment does not switch a service-account
	// install over to Connect.
	t.Setenv("OP_CONNECT_HOST", "https://elsewhere.example.test")
	t.Setenv("OP_CONNECT_TOKEN", "eyJenv")
	if _, err := onepasswordBackendFrom(opCreds(nil), map[string]string{}); !errors.Is(err, errNotConfigured) {
		t.Errorf("the environment chose the mode: %v", err)
	}
	// In Connect mode the environment's pair wins as a pair.
	be, err = onepasswordBackendFrom(stored, connect)
	if c, ok := be.(*onepassword.Connect); err != nil || !ok || c.Host() != "https://elsewhere.example.test" {
		t.Errorf("the environment's pair: %T %v", be, err)
	}
	// Half a pair is not used: the stored token never goes to an address that
	// only the environment named.
	t.Setenv("OP_CONNECT_TOKEN", "")
	be, err = onepasswordBackendFrom(stored, connect)
	if c, ok := be.(*onepassword.Connect); err != nil || !ok || c.Host() != "https://connect.example.test:8443" {
		t.Errorf("half a pair from the environment: %T %v", be, err)
	}
}

// Desktop sign-in reaches the whole account, so it will not start without
// the vault allowlist.
func TestOnePasswordDesktopSignInNeedsTheVaultAllowlist(t *testing.T) {
	_, err := onepasswordBackendFrom(opCreds(nil), map[string]string{"auth": "desktop", "account": "my.1password.com"})
	if !errors.Is(err, errNotConfigured) || !strings.Contains(err.Error(), "vaults") {
		t.Errorf("desktop with no allowlist: %v", err)
	}
}

// With desktop sign-in only a turn on the local surface has a person to
// approve: cron and the relays are refused before op is started.
func TestOnePasswordDesktopSignInIsForTheLocalSurfaceOnly(t *testing.T) {
	for surface, want := range map[string]bool{surfaceLocal: true, surfaceCronExec: false, surfaceCronShell: false, surfaceDiscord: false, surfaceIMessage: false} {
		if got := onepasswordPresent(withExecSurface(context.Background(), surface)); got != want {
			t.Errorf("%s: present=%v, want %v", surface, got, want)
		}
	}
	if !onepasswordPresent(context.Background()) {
		t.Errorf("an unstamped turn is the local surface")
	}
}

func TestOnePasswordSchemaAsksOnlyForTheChosenModesFields(t *testing.T) {
	k := onepasswordSkillKind()
	shown := func(settings map[string]string) string {
		var keys []string
		for _, f := range k.Fields {
			if f.ShowIf == nil || f.ShowIf(settings) {
				keys = append(keys, f.Key)
			}
		}
		return strings.Join(keys, ",")
	}
	for auth, want := range map[string]string{
		"":                "auth,token,vaults,op_path,allow_write",
		"service_account": "auth,token,vaults,op_path,allow_write",
		"connect":         "auth,connect_host,connect_token,vaults,allow_write",
		"desktop":         "auth,account,vaults,op_path,allow_write",
	} {
		if got := shown(map[string]string{"auth": auth}); got != want {
			t.Errorf("auth=%q asks for %s, want %s", auth, got, want)
		}
	}
	if err := validateOnePasswordToken("eyJabc"); err == nil || !strings.Contains(err.Error(), "Connect") {
		t.Errorf("a Connect token in the service account field: %v", err)
	}
	if err := validateConnectToken("ops_abc"); err == nil {
		t.Errorf("a service account token in the Connect field is accepted")
	}
	for in, ok := range map[string]bool{"https://c.example.test:8443": true, "http://localhost:8080": true, "": true, "c.example.test": false, "ftp://c.example.test": false} {
		if err := validateConnectHost(in); (err == nil) != ok {
			t.Errorf("connect host %q: %v", in, err)
		}
	}
}

func TestOnePasswordDoctorHelpers(t *testing.T) {
	for host, want := range map[string]bool{
		"http://10.0.0.5:8080":             true,
		"http://connect.example.test:8080": true,
		"http://localhost:8080":            false,
		"http://127.0.0.1:8080":            false,
		"http://[::1]:8080":                false,
		"https://connect.example.test":     false,
	} {
		if got := plainHTTPOffHost(host); got != want {
			t.Errorf("plainHTTPOffHost(%q) = %v, want %v", host, got, want)
		}
	}
	accounts := []onepassword.Account{{URL: "team.1password.com", ID: "A1", UserID: "U1"}}
	for want, ok := range map[string]bool{"": true, "team.1password.com": true, "https://Team.1password.com/": true, "a1": true, "U1": true, "other.1password.com": false} {
		if got := accountOffered(accounts, want); got != ok {
			t.Errorf("accountOffered(%q) = %v, want %v", want, got, ok)
		}
	}
	if accountOffered(nil, "") {
		t.Errorf("an app with no accounts offers one")
	}
}

// An install is configured when the chosen mode's own required fields are
// set: another mode's token is not missing. A config from before the auth
// setting (a token and nothing else) is still a configured service account.
func TestOnePasswordIsConfiguredForTheChosenModeOnly(t *testing.T) {
	kind := onepasswordSkillKind()
	kind.Probe = func(context.Context, *credentials, map[string]string, func() error) (string, error) { return "ok", nil }
	kinds := map[string]skillKind{"onepassword": kind}
	for name, args := range map[string][]string{
		"service account": {"onepassword", "token=ops_test_token_1234567890"},
		"connect":         {"onepassword", "auth=connect", "connect_host=https://connect.example.test:8443", "connect_token=eyJtest"},
		"desktop":         {"onepassword", "auth=desktop", "vaults=Ops"},
	} {
		sc := newSkillCfgControl(t.TempDir(), kinds, nil)
		if _, err := sc.SkillConfigure(args, true); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !sc.isConfigured("onepassword", kind) {
			t.Errorf("%s: saved, but reported as not configured", name)
		}
	}
	if sc := newSkillCfgControl(t.TempDir(), kinds, nil); sc.isConfigured("onepassword", kind) {
		t.Errorf("nothing saved, but reported as configured")
	}
	// Connect chosen with no address or token is refused at configure time.
	sc := newSkillCfgControl(t.TempDir(), kinds, nil)
	if _, err := sc.SkillConfigure([]string{"onepassword", "auth=connect"}, true); err == nil {
		t.Errorf("Connect with no address or token was saved")
	}
}
