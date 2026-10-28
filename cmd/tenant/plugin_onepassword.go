package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
// credentials, all through the op command-line tool with a service account
// token. The token is a stored secret or $OP_SERVICE_ACCOUNT_TOKEN; there is
// deliberately no --onepassword-token flag (shell history, the task XML).
func onepasswordPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "onepassword",
		Events:    onepassword.Events,
		Connector: true,
		Keys: []keySpec{{CredID: "skill:onepassword:token", Name: "1Password service account token", Category: "Integration",
			EnvVars: []string{"OP_SERVICE_ACCOUNT_TOKEN"}, Kind: keyDirect}},
		Flags:       bindOnePasswordFlags,
		On:          func(pf *pluginFlags) bool { return onepasswordFlagsOf(pf).on },
		ApplyConfig: applyOnePasswordConfig,
		ExpandPaths: func(pf *pluginFlags) { f := onepasswordFlagsOf(pf); f.opPath = expandPath(f.opPath) },
		Launch:      launchOnePassword,
		Activate:    activateOnePassword,
		Title:       "1Password (vaults, items and secrets through the op CLI)",
		Version:     1,
		Tools:       func() []model.ToolSpec { return onepassword.NewDispatcher(nil, onepassword.Policy{}).Tools() },
		Configure:   onepasswordSkillKind,
		SetupOrder:  10,
		Actions: map[string]string{
			onepassword.ActionReveal:  catEgress,      // a secret enters the model's context
			onepassword.ActionWrite:   catWrite,       // op_create, op_edit
			onepassword.ActionArchive: catDestructive, // op_delete; a nil confirm denies it
		},
		StubHint: "run `/configure onepassword` (a service account token), then `/enable onepassword`",
		Doctor: []doctorCheck{
			{"onepassword (if enabled)", checkOnePassword},
		},
		CLI: []*cliCommand{
			{Names: []string{"onepassword"}, Section: "plugins", Run: cmdOnePassword,
				Summary: "list 1Password vaults and items (read-only)",
				Rows:    []cmdreg.Row{{Syntax: "onepassword whoami | vaults | items <vault>", Desc: "who the token is, the vaults it sees, the items in one (never a secret value)"}}},
		},
	}
}

// onepasswordFlags are the plugin's launch flags. opPath and vaults are
// settings folded in by ApplyConfig; only the switches are flags.
type onepasswordFlags struct {
	on         bool
	allowWrite bool
	opPath     string
	vaults     string
}

func onepasswordFlagsOf(pf *pluginFlags) *onepasswordFlags {
	return flagsOf[onepasswordFlags](pf, "onepassword")
}

func bindOnePasswordFlags(fs *flag.FlagSet) any {
	f := &onepasswordFlags{}
	fs.BoolVar(&f.on, "onepassword", false, "enable the 1Password tools (a stored service account token or $OP_SERVICE_ACCOUNT_TOKEN)")
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
	f.opPath = firstNonEmpty(f.opPath, s.get("op_path"))
	f.vaults = firstNonEmpty(f.vaults, s.get("vaults"))
}

// onepasswordToken is the token the plugin runs with: the environment wins
// over the stored secret, as the integrations page says for every plugin.
func onepasswordToken(cfgDir string) string {
	if t := strings.TrimSpace(os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")); t != "" {
		return t
	}
	creds, _ := loadCredentials(cfgDir)
	return strings.TrimSpace(creds.Get(skillSecretID("onepassword", "token")))
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

// buildOnePassword makes the plugin from a token and its settings.
func buildOnePassword(e *pluginEnv, token, opPath, vaults string, allowWrite bool) (plugin, error) {
	if token == "" {
		return nil, fmt.Errorf("onepassword plugin: %w yet - no token; run `/configure onepassword`", errNotConfigured)
	}
	cli, err := onepassword.NewCLI(opPath, token)
	if err != nil {
		return nil, fmt.Errorf("onepassword plugin: %w", err)
	}
	return onepassword.NewDispatcher(cli, onepassword.Policy{
		AllowWrite: e.gateAllow(allowWrite),
		Confirm:    e.confirm,
		Vaults:     splitVaults(vaults),
		Revealed:   onepasswordRevealed,
	}), nil
}

// launchOnePassword builds the plugin from the launch flags (and the saved
// settings ApplyConfig folded in).
func launchOnePassword(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := onepasswordFlagsOf(e.pf)
	if !f.on {
		return nil, nil, nil
	}
	p, err := buildOnePassword(e, onepasswordToken(e.c.cfgDir), f.opPath, f.vaults, f.allowWrite)
	if err != nil {
		return nil, nil, err
	}
	return []plugin{p}, nil, nil
}

// activateOnePassword builds the plugin from /configure-saved settings on
// its first /enable, so enabling it mid-session needs no relaunch.
func activateOnePassword(_ context.Context, e *pluginEnv) (plugin, func(), error) {
	lc, err := loadLaunchConfig(e.c.cfgDir)
	if err != nil {
		return nil, nil, fmt.Errorf("onepassword plugin: load config: %w", err)
	}
	var settings map[string]string
	if sk, ok := lc.Skills["onepassword"]; ok && sk != nil {
		settings = sk.Settings
	}
	allowWrite := onepasswordFlagsOf(e.pf).allowWrite || settings["allow_write"] == "true"
	p, err := buildOnePassword(e, onepasswordToken(e.c.cfgDir), settings["op_path"], settings["vaults"], allowWrite)
	if err != nil {
		return nil, nil, err
	}
	return p, func() {}, nil
}

// checkOnePassword is the doctor check: the CLI is found and the token is
// accepted. Offline is a WARN, so an offline diagnostic still passes the
// checks that matter.
func checkOnePassword(ctx context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "onepassword plugin not configured"}
	}
	sk := e.lc.Skills["onepassword"]
	if sk == nil || !sk.Enabled {
		return checkResult{Status: statusSkip, Detail: "onepassword plugin not enabled"}
	}
	token := onepasswordToken(e.c.cfgDir)
	if token == "" {
		return checkResult{Status: statusFail, Detail: "onepassword enabled but no service account token is stored",
			Fix: "run `/configure onepassword` or set $OP_SERVICE_ACCOUNT_TOKEN"}
	}
	cli, err := onepassword.NewCLI(sk.Settings["op_path"], token)
	if err != nil {
		return checkResult{Status: statusFail, Detail: err.Error(),
			Fix: "install the 1Password CLI, or set op_path in `/configure onepassword` to the full path of op"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, err := cli.WhoAmI(probeCtx)
	switch {
	case err == nil:
		detail := fmt.Sprintf("%s signed in at %s via %s", strings.ToLower(firstNonEmpty(id.Type, "account")), id.URL, cli.Path())
		if v := strings.TrimSpace(sk.Settings["vaults"]); v != "" {
			detail += "; vaults limited to " + v
		}
		return checkResult{Status: statusOK, Detail: detail}
	case errors.Is(err, onepassword.ErrAuth):
		return checkResult{Status: statusFail, Detail: err.Error(), Fix: "store a current service account token with `/configure onepassword`"}
	default:
		return checkResult{Status: statusWarn, Detail: "1Password not reachable: " + err.Error()}
	}
}
