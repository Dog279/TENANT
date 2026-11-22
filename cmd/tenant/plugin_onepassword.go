package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"tenant/internal/cmdreg"
	"tenant/internal/logs"
	"tenant/internal/model"
	"tenant/internal/plugins/onepassword"
)

// onepasswordPlugin declares 1Password: the agent lists vaults and items,
// reads one secret at a time through an op:// reference, and keeps new
// credentials. It signs in one of three ways (the auth setting): a service
// account token through the op command-line tool, a self-hosted Connect
// server's REST API, or the 1Password desktop app on this machine, which asks
// the person there. A token is a stored secret or an environment variable;
// there is deliberately no token flag (shell history, the task XML).
func onepasswordPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "onepassword",
		Events:    onepassword.Events,
		Connector: true,
		Keys: []keySpec{
			{CredID: "skill:onepassword:token", Name: "1Password service account token", Category: "Integration",
				EnvVars: []string{"OP_SERVICE_ACCOUNT_TOKEN"}, Kind: keyDirect},
			{CredID: "skill:onepassword:connect_token", Name: "1Password Connect server token", Category: "Integration",
				EnvVars: []string{"OP_CONNECT_TOKEN"}, Kind: keyDirect},
		},
		Flags:       bindOnePasswordFlags,
		On:          func(pf *pluginFlags) bool { return onepasswordFlagsOf(pf).on },
		ApplyConfig: applyOnePasswordConfig,
		ExpandPaths: func(pf *pluginFlags) { f := onepasswordFlagsOf(pf); f.opPath = expandPath(f.opPath) },
		Launch:      launchOnePassword,
		Activate:    activateOnePassword,
		Title:       "1Password (vaults, items and secrets; a service account, a Connect server or the desktop app)",
		Version:     1,
		Tools:       func() []model.ToolSpec { return onepassword.NewDispatcher(nil, onepassword.Policy{}).Tools() },
		Configure:   onepasswordSkillKind,
		SetupOrder:  10,
		Actions: map[string]string{
			onepassword.ActionReveal:  catEgress,      // a secret enters the model's context
			onepassword.ActionWrite:   catWrite,       // op_create, op_edit
			onepassword.ActionArchive: catDestructive, // op_delete; a nil confirm denies it
		},
		StubHint: "run `/configure onepassword` (pick how it signs in), then `/enable onepassword`",
		Doctor: []doctorCheck{
			{"onepassword (if enabled)", checkOnePassword},
		},
		CLI: []*cliCommand{
			{Names: []string{"onepassword"}, Section: "plugins", Run: cmdOnePassword,
				Summary: "list 1Password vaults and items (read-only)",
				Rows:    []cmdreg.Row{{Syntax: "onepassword whoami | vaults | items <vault>", Desc: "who the sign-in is, the vaults it sees, the items in one (never a secret value)"}}},
		},
	}
}

// The ways the plugin signs in: the auth setting.
const (
	opAuthService = "service_account" // a service account token through op (the default)
	opAuthConnect = "connect"         // a self-hosted Connect server's REST API
	opAuthDesktop = "desktop"         // the 1Password app on this machine asks the person there
)

// onepasswordAuth is the sign-in mode a settings map names. Blank is a
// service account: what every install from before the setting has.
func onepasswordAuth(settings map[string]string) string {
	switch a := strings.ToLower(strings.TrimSpace(settings["auth"])); a {
	case opAuthConnect, opAuthDesktop:
		return a
	}
	return opAuthService
}

// onepasswordFlags are the plugin's launch flags. Everything but the two
// switches is a setting folded in by ApplyConfig.
type onepasswordFlags struct {
	on          bool
	allowWrite  bool
	auth        string
	account     string
	connectHost string
	opPath      string
	vaults      string
}

// settings is the flags as the settings map the builders read.
func (f *onepasswordFlags) settings() map[string]string {
	return map[string]string{"auth": f.auth, "account": f.account, "connect_host": f.connectHost, "op_path": f.opPath, "vaults": f.vaults}
}

func onepasswordFlagsOf(pf *pluginFlags) *onepasswordFlags {
	return flagsOf[onepasswordFlags](pf, "onepassword")
}

func bindOnePasswordFlags(fs *flag.FlagSet) any {
	f := &onepasswordFlags{}
	fs.BoolVar(&f.on, "onepassword", false, "enable the 1Password tools (signed in as `/configure onepassword` set: a service account, a Connect server or the desktop app)")
	fs.BoolVar(&f.allowWrite, "onepassword-allow-write", false, "permit op_create/op_edit without a per-action approval (op_delete always asks)")
	return f
}

func applyOnePasswordConfig(pf *pluginFlags, s savedConfig) {
	f := onepasswordFlagsOf(pf)
	if !s.set["onepassword"] {
		f.on = true
	}
	if !s.set["onepassword-allow-write"] && s.get("allow_write") == "true" {
		f.allowWrite = true
	}
	f.auth = firstNonEmpty(f.auth, s.get("auth"))
	f.account = firstNonEmpty(f.account, s.get("account"))
	f.connectHost = firstNonEmpty(f.connectHost, s.get("connect_host"))
	f.opPath = firstNonEmpty(f.opPath, s.get("op_path"))
	f.vaults = firstNonEmpty(f.vaults, s.get("vaults"))
}

// splitVaults parses the comma-separated vaults setting.
func splitVaults(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// onepasswordRevealed hands a value op_read returned to the log book's
// scrubber, under the reference's name, so no log line ever carries it.
func onepasswordRevealed(reference, value string) {
	logs.AddSecrets(map[string]string{"onepassword:" + reference: value})
}

// onepasswordBackend builds the backend the saved settings name.
func onepasswordBackend(cfgDir string, settings map[string]string) (onepassword.Backend, error) {
	creds, _ := loadCredentials(cfgDir)
	return onepasswordBackendFrom(creds, settings)
}

// onepasswordBackendFrom builds the backend for the sign-in mode in settings.
// The mode comes from the settings alone: an environment variable supplies
// the credential for the mode chosen (and wins over the stored one, as the
// integrations page says for every plugin), it never chooses the mode.
func onepasswordBackendFrom(creds *credentials, settings map[string]string) (onepassword.Backend, error) {
	switch onepasswordAuth(settings) {
	case opAuthConnect:
		// The address and the token are one pair: both from the environment,
		// or both as stored, so a stored token is never sent to an address
		// that only an environment variable named.
		host, token := strings.TrimSpace(os.Getenv("OP_CONNECT_HOST")), strings.TrimSpace(os.Getenv("OP_CONNECT_TOKEN"))
		if host == "" || token == "" {
			host, token = strings.TrimSpace(settings["connect_host"]), strings.TrimSpace(creds.Get(skillSecretID("onepassword", "connect_token")))
		}
		if host == "" || token == "" {
			return nil, fmt.Errorf("%w yet - no Connect server address or token; run `/configure onepassword`", errNotConfigured)
		}
		c, err := onepassword.NewConnect(host, token)
		if err != nil {
			return nil, err
		}
		return c, nil
	case opAuthDesktop:
		// The app's approval covers every vault the person can see, their
		// private one included. The allowlist is the only fence, so it is
		// not optional here.
		if len(splitVaults(settings["vaults"])) == 0 {
			return nil, fmt.Errorf("%w yet - desktop sign-in reaches every vault of the account, so the vaults the agent may use must be named; run `/configure onepassword`", errNotConfigured)
		}
		cli, err := onepassword.NewDesktopCLI(settings["op_path"], settings["account"])
		if err != nil {
			return nil, err
		}
		return cli, nil
	}
	token := strings.TrimSpace(os.Getenv("OP_SERVICE_ACCOUNT_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(creds.Get(skillSecretID("onepassword", "token")))
	}
	if token == "" {
		return nil, fmt.Errorf("%w yet - no service account token; run `/configure onepassword`", errNotConfigured)
	}
	cli, err := onepassword.NewCLI(settings["op_path"], token)
	if err != nil {
		return nil, err
	}
	return cli, nil
}

// buildOnePassword makes the plugin from its settings.
func buildOnePassword(e *pluginEnv, settings map[string]string, allowWrite bool) (plugin, error) {
	be, err := onepasswordBackend(e.c.cfgDir, settings)
	if err != nil {
		return nil, fmt.Errorf("onepassword plugin: %w", err)
	}
	policy := onepassword.Policy{
		AllowWrite: e.gateAllow(allowWrite),
		Confirm:    e.confirm,
		Vaults:     splitVaults(settings["vaults"]),
		Revealed:   onepasswordRevealed,
	}
	if onepasswordAuth(settings) == opAuthDesktop {
		policy.Present = onepasswordPresent
	}
	return onepassword.NewDispatcher(be, policy), nil
}

// onepasswordPresent reports whether a turn has a person at this machine,
// which desktop sign-in needs: the 1Password app asks whoever is there. A
// cron job or a Discord or iMessage turn has nobody, and must not raise a
// prompt on an empty desk.
func onepasswordPresent(ctx context.Context) bool {
	return execSurfaceFrom(ctx) == surfaceLocal
}

// launchOnePassword builds the plugin from the launch flags (and the saved
// settings ApplyConfig folded in).
func launchOnePassword(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := onepasswordFlagsOf(e.pf)
	if !f.on {
		return nil, nil, nil
	}
	p, err := buildOnePassword(e, f.settings(), f.allowWrite)
	if err != nil {
		return nil, nil, err
	}
	return []plugin{p}, nil, nil
}

// onepasswordSettings is the plugin's saved settings; nil when it has none.
func onepasswordSettings(lc *launchConfig) map[string]string {
	if lc == nil {
		return nil
	}
	if sk, ok := lc.Skills["onepassword"]; ok && sk != nil {
		return sk.Settings
	}
	return nil
}

// activateOnePassword builds the plugin from /configure-saved settings on
// its first /enable, so enabling it mid-session needs no relaunch.
func activateOnePassword(_ context.Context, e *pluginEnv) (plugin, func(), error) {
	lc, err := loadLaunchConfig(e.c.cfgDir)
	if err != nil {
		return nil, nil, fmt.Errorf("onepassword plugin: load config: %w", err)
	}
	settings := onepasswordSettings(lc)
	allowWrite := onepasswordFlagsOf(e.pf).allowWrite || settings["allow_write"] == "true"
	p, err := buildOnePassword(e, settings, allowWrite)
	if err != nil {
		return nil, nil, err
	}
	return p, func() {}, nil
}

// onepasswordIdentity words who a sign-in is, for doctor, the configure
// probe and `tenant onepassword whoami`.
func onepasswordIdentity(be onepassword.Backend, id onepassword.Identity) string {
	switch b := be.(type) {
	case *onepassword.Connect:
		return "Connect server at " + b.Host()
	case *onepassword.CLI:
		kind := strings.ToLower(firstNonEmpty(id.Type, "account"))
		if b.Desktop() {
			kind += " through the desktop app"
		}
		if id.Email != "" {
			kind += " " + id.Email
		}
		return fmt.Sprintf("%s at %s via %s", kind, firstNonEmpty(id.URL, "1Password"), b.Path())
	}
	return firstNonEmpty(id.URL, "1Password")
}

// plainHTTPOffHost reports whether a Connect address is plain http to
// another machine: the token would cross the network unencrypted.
func plainHTTPOffHost(host string) bool {
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}

// accountOffered reports whether the 1Password app holds the account the
// settings name (a sign-in address or an id); no name means any one account.
func accountOffered(accounts []onepassword.Account, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return len(accounts) > 0
	}
	trim := func(s string) string {
		s = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(s), "https://"), "http://")
		return strings.TrimSuffix(s, "/")
	}
	for _, a := range accounts {
		if trim(want) == trim(a.URL) || strings.EqualFold(want, a.ID) || strings.EqualFold(want, a.UserID) {
			return true
		}
	}
	return false
}

// checkOnePassword is the doctor check: the sign-in the settings name works.
// Offline is a WARN, so an offline diagnostic still passes the checks that
// matter. With desktop sign-in it asks only which accounts the app offers:
// doctor must never put an approval prompt on someone's screen.
func checkOnePassword(ctx context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "onepassword plugin not configured"}
	}
	sk := e.lc.Skills["onepassword"]
	if sk == nil || !sk.Enabled {
		return checkResult{Status: statusSkip, Detail: "onepassword plugin not enabled"}
	}
	be, err := onepasswordBackend(e.c.cfgDir, sk.Settings)
	switch {
	case errors.Is(err, onepassword.ErrCLINotFound):
		return checkResult{Status: statusFail, Detail: err.Error(),
			Fix: "install the 1Password CLI, or set op_path in `/configure onepassword` to the full path of op"}
	case err != nil:
		return checkResult{Status: statusFail, Detail: "onepassword enabled but " + err.Error(), Fix: "run `/configure onepassword`"}
	}
	limited := ""
	if v := strings.TrimSpace(sk.Settings["vaults"]); v != "" {
		limited = "; vaults limited to " + v
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if cli, ok := be.(*onepassword.CLI); ok && cli.Desktop() {
		accounts, err := cli.Accounts(probeCtx)
		switch {
		case err != nil:
			return checkResult{Status: statusFail, Detail: err.Error(),
				Fix: "open 1Password on this machine and turn on Settings, Developer, `Integrate with 1Password CLI`; Tenant must run in the same desktop session as the app"}
		case !accountOffered(accounts, sk.Settings["account"]):
			return checkResult{Status: statusFail, Detail: fmt.Sprintf("the 1Password app offers %d account(s) and none is %q", len(accounts), sk.Settings["account"]),
				Fix: "add the account to the 1Password app, or correct `account` in `/configure onepassword`"}
		case strings.TrimSpace(sk.Settings["account"]) == "" && len(accounts) > 1:
			return checkResult{Status: statusWarn, Detail: fmt.Sprintf("the 1Password app holds %d accounts and none is named; op will not know which to use", len(accounts)),
				Fix: "set `account` in `/configure onepassword` to the account's sign-in address"}
		}
		return checkResult{Status: statusOK, Detail: fmt.Sprintf("desktop app sign-in via %s (each use is approved in the app)%s", cli.Path(), limited)}
	}
	id, err := be.WhoAmI(probeCtx)
	switch {
	case err == nil:
		if c, ok := be.(*onepassword.Connect); ok && plainHTTPOffHost(c.Host()) {
			return checkResult{Status: statusWarn, Detail: onepasswordIdentity(be, id) + limited + "; the token crosses the network unencrypted",
				Fix: "serve the Connect server over TLS (its OP_TLS_* settings) and use an https address"}
		}
		return checkResult{Status: statusOK, Detail: onepasswordIdentity(be, id) + limited}
	case errors.Is(err, onepassword.ErrAuth):
		return checkResult{Status: statusFail, Detail: err.Error(), Fix: "store a current token with `/configure onepassword`"}
	default:
		return checkResult{Status: statusWarn, Detail: "1Password not reachable: " + err.Error()}
	}
}
