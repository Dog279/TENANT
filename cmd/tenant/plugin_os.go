package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"tenant/internal/cmdreg"
	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/model/backend/claudecode"
	"tenant/internal/peering"
	"tenant/internal/plugins/osys"
)

// osPlugin declares the OS plugin: machine info, the file tools and gated
// shell exec. It needs no config, so the launch path always registers it
// (enabled only with --os); it has no stub.
func osPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "os",
		Events:      osys.Events,
		Flags:       bindOSFlags,
		On:          func(pf *pluginFlags) bool { return osFlagsOf(pf).on },
		ApplyConfig: applyOSConfig,
		Grants:      osGrants,
		Launch:      launchOS,
		Eval:        evalOS,
		Title:       "OS access (sysinfo, files, gated shell)",
		Version:     1,
		Tools:       func() []model.ToolSpec { return osys.NewDispatcher(nil, osys.Policy{}).Tools() },
		Configure: func() skillKind {
			return skillKind{ID: "os", Label: "OS access (sysinfo, files, gated shell)", Wired: true}
		},
		SetupOnly:  true,
		SetupOrder: 9,
		Actions: map[string]string{
			"os_exec":  catExec,
			"os_write": catWrite, "os_append": catWrite, "os_edit": catWrite, "os_mkdir": catWrite,
			"os_exec_dangerous": catDestructive,
			"os_write_outside":  catDestructive, // a write outside the roots asks (TEN-489)
		},
		Doctor: []doctorCheck{
			// TEN-25: os_processes shells out to `ps` / `tasklist`. Missing on
			// minimal containers; warn early so fitness-019 doesn't fail for an
			// infrastructure reason.
			{"os_processes tool", checkOSProcesses},
		},
		CLI: []*cliCommand{
			{Names: []string{"os"}, Section: "plugins", Run: cmdOS,
				Summary: "the agent inspects the machine and runs shell commands",
				Rows:    []cmdreg.Row{{Syntax: `os "<task>"`, Desc: "the agent inspects the machine + runs shell commands (read; --allow-exec to run)"}}},
		},
	}
}

// osFlags are the OS plugin's launch flags.
type osFlags struct {
	on         bool
	allowExec  bool
	allowWrite bool
	writeRoots string
	denyRoots  string
}

func osFlagsOf(pf *pluginFlags) *osFlags { return flagsOf[osFlags](pf, "os") }

func bindOSFlags(fs *flag.FlagSet) any {
	f := &osFlags{}
	fs.BoolVar(&f.on, "os", false, "enable the OS plugin (sysinfo, file read, dir list, processes, gated shell exec)")
	fs.BoolVar(&f.allowExec, "os-allow-exec", false, "permit os_exec (run shell commands) — destructive ones still need confirm")
	fs.BoolVar(&f.allowWrite, "os-allow-write", false, "permit os_write/os_edit/os_append/os_make_dir (file writes)")
	fs.StringVar(&f.writeRoots, "os-write-roots", "", "comma-separated directories the file tools may WRITE to (default: Tenant's config and data dirs, the operator's Desktop/Documents/Downloads, and the temp dir). Tenant's own control and secret files stay off limits either way.")
	fs.StringVar(&f.denyRoots, "os-deny-roots", "", "comma-separated directories the file tools may never touch, read OR write (default: Windows/Program Files/ProgramData)")
	return f
}

func applyOSConfig(pf *pluginFlags, s savedConfig) {
	if !s.set["os"] {
		osFlagsOf(pf).on = true
	}
}

func osGrants(pf *pluginFlags) map[string]string {
	f := osFlagsOf(pf)
	g := map[string]string{}
	if f.allowExec {
		g[catExec] = "--os-allow-exec"
	}
	if f.allowWrite {
		g[catWrite] = "--os-allow-write"
	}
	return g
}

// launchOS always registers the OS plugin — it needs no config, so it is
// visible in /tools and `/enable os` activates it instantly; it starts
// enabled only with --os. os_exec runs through the execution-backend seam
// (execprofiles.go): the turn's surface picks its exec profile per call, and a
// sandbox refusal escalates through the same origin-routed confirm as every
// other gated action.
func launchOS(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	svc, err := osys.Open(osExecConfig(e.c.cfgDir, e.c.dataDir, e.confirm))
	if err != nil {
		return nil, nil, nil // no OS service here: the plugin is simply absent
	}
	f := osFlagsOf(e.pf)
	return []plugin{osys.NewDispatcher(svc, osys.Policy{
		AllowExec:  e.gateAllow(f.allowExec),
		AllowWrite: e.gateAllow(f.allowWrite),
		Confirm:    e.confirm,
		Roots:      resolveOSRoots(e.pf, e.c.cfgDir, e.c.dataDir, e.c.lc),
	})}, svc.Close, nil // the background processes die with Tenant (TEN-490)
}

// evalOS: the OS plugin is pure Go and always available; eval needs real
// execution and file writes.
func evalOS(pf *pluginFlags, _ *commonFlags, explicit map[string]bool) bool {
	f := osFlagsOf(pf)
	if explicit["os"] || f.on {
		return false
	}
	f.on, f.allowExec, f.allowWrite = true, true, true
	return true
}

// checkOSProcesses verifies the platform-specific process-listing tool
// (`ps` on Unix, `tasklist` on Windows) is on PATH. The os_processes
// agent tool shells out to it (see internal/plugins/osys/osys.go:270);
// on minimal containers (Alpine, distroless, scratch) `ps` is commonly
// absent and the agent's process-listing tool silently fails at first
// use with "executable file not found in $PATH". Doctor surfaces it
// upfront so the operator can apk add procps (or equivalent) before
// running fitness evals.
//
// TEN-25 motivation: fitness-019-os-processes depends on this binary;
// if it's missing the task fails for an infrastructure reason, not an
// agent-skill reason.
func checkOSProcesses(_ context.Context, _ *doctorEnv) checkResult {
	tool := "ps"
	if runtime.GOOS == "windows" {
		tool = "tasklist"
	}
	path, err := exec.LookPath(tool)
	if err != nil {
		fix := "install procps (Alpine: apk add procps; Debian: apt install procps)"
		if runtime.GOOS == "windows" {
			fix = "tasklist ships with Windows by default — check PATH"
		}
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("%s not found on PATH — os_processes tool will fail at runtime", tool),
			Fix:    fix}
	}
	return checkResult{Status: statusOK, Detail: tool + " at " + path}
}

// resolveOSRoots builds the file-tool boundary. The write roots default to
// where the agent legitimately works: Tenant's own config and data dirs, the
// operator's Desktop/Documents/Downloads and the temp dir. Never a PARENT of
// the config or data dir: with default dirs that is the OS-wide config root
// (~/.config, %AppData%, ~/Library/Application Support), which holds autostart
// entries and other programs' state, and on a C:\Tenant server it holds the
// binary and the claude CLI's config. Nothing Tenant does needs it: cron, MCP
// and peer changes persist in-process, wiki_save writes the wiki, and memory
// and skills have their own stores.
//
// Deny always includes the platform defaults even when the operator supplies
// their own list: an allowlist typo should not silently expose System32.
// Protect and Secret are always Tenant's own (fileToolGuards), whatever the
// write roots say.
//
// This bounds the FILE tools only. os_exec takes an opaque command string and
// is bounded by the OS account Tenant runs as, not by this list.
func resolveOSRoots(pf *pluginFlags, cfgDir, dataDir string, lc *launchConfig) osys.Roots {
	split := func(v string) []string {
		var out []string
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	deny := append(osys.DefaultDeny(), split(osFlagsOf(pf).denyRoots)...)

	write := split(osFlagsOf(pf).writeRoots)
	if len(write) == 0 {
		seen := map[string]bool{}
		add := func(p string) {
			if p == "" || seen[strings.ToLower(p)] {
				return
			}
			seen[strings.ToLower(p)] = true
			write = append(write, p)
		}
		add(cfgDir)
		add(dataDir)
		if home, err := os.UserHomeDir(); err == nil {
			for _, sub := range []string{"Desktop", "Documents", "Downloads"} {
				add(filepath.Join(home, sub))
			}
		}
		if tmp := os.TempDir(); tmp != "" {
			add(tmp)
		}
	}
	protect, secret := fileToolGuards(cfgDir, dataDir, pf, lc)
	return osys.Roots{Deny: deny, Write: write, Protect: protect, Secret: secret}
}

// fileToolGuards lists what the file tools may never write (protect) and may
// never read or write (secret), on any surface and whatever the write mode.
// Tenant writes all of it in-process, and no agent flow needs the tools for
// any of it.
//
// Protected, because Tenant applies an edit without asking, or runs it:
//   - settings.<agent>.json, for every agent sharing the config dir, and its
//     temp files: permission modes, tool rules and exec profiles, re-read
//     within seconds (settingswatch.go);
//   - soul/: the persona in every prompt, which is the operator's alone;
//   - peer-cert.pem: this instance's peer identity;
//   - the claudecode spend ledger: the weekly budget rail;
//   - the Tenant binary;
//   - on Windows, the PowerShell profile and module folders under Documents,
//     which run in the operator's next PowerShell session.
//
// Secret, because a turn could send what they hold anywhere:
//   - config.json: the dashboard's auth token, beside cron jobs, MCP servers,
//     providers and relay and iMessage permissions (persistLaunchConfig folds
//     a hand edit into the next in-process save);
//   - every file on managedSecrets, the authoritative list doctor audits too
//     (TEN-337): credentials.json with every API key and skill secret; the
//     dashboard-auth-token; peers.json and the peer key and cert, who may call
//     in and with what token; the OAuth token files (Atlassian, GitHub, X in
//     the data dir, and the mcp/ caches) and the gsuite OAuth client;
//   - the files the operator pointed a plugin at (the gsuite service-account
//     key and OAuth client) and the dashboard TLS key, when set;
//   - the claude CLI's config ($CLAUDE_CONFIG_DIR, else ~/.claude and
//     ~/.claude.json): its login, and the hooks and MCP servers it runs;
//   - the credential stores and browser profiles the exec sandbox denies to
//     confined commands (credentialStoreRoots).
func fileToolGuards(cfgDir, dataDir string, pf *pluginFlags, lc *launchConfig) (protect, secret []string) {
	add := func(list *[]string, paths ...string) {
		for _, p := range paths {
			if p = strings.TrimSpace(p); p != "" {
				*list = append(*list, expandHome(p))
			}
		}
	}
	if cfgDir != "" {
		settings := settingsPath(cfgDir, "*")
		peerCert, _ := peering.CertPaths(cfgDir)
		add(&protect, settings, settings+".*.tmp", soul.SoulDir(cfgDir), peerCert, chainAnchorPath(cfgDir))
		// Plugin settings that decide what the agent may do (the GitHub merge
		// rules, TEN-432) are trust roots like the approval settings: the agent
		// must not loosen a gate it passes through.
		for _, f := range pluginConfigFiles(cfgDir) {
			add(&protect, f, f+".*")
		}
		// The mcp dir as a whole also covers caches created after this list.
		add(&secret, launchConfigPath(cfgDir), mcpCacheDir(cfgDir))
	}
	// Every file on the authoritative secret list, config and data dirs both
	// (TEN-337). A listing error still leaves every other entry.
	managed, _ := managedSecrets(cfgDir, dataDir)
	for _, m := range managed {
		add(&secret, m.path)
	}
	add(&protect, claudecode.LedgerPath(), exePath())
	home, homeErr := os.UserHomeDir()
	if homeErr == nil && runtime.GOOS == "windows" {
		add(&protect, filepath.Join(home, "Documents", "WindowsPowerShell"), filepath.Join(home, "Documents", "PowerShell"))
	}
	for _, d := range plugins().All() {
		if d.PointedSecrets == nil {
			continue
		}
		var settings map[string]string
		if lc != nil && lc.Skills[d.ID] != nil {
			settings = lc.Skills[d.ID].Settings
		}
		add(&secret, d.PointedSecrets(pf, settings)...)
	}
	if lc != nil {
		add(&secret, lc.Dashboard.TLSKey)
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		add(&secret, dir)
	} else if homeErr == nil {
		add(&secret, filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json"))
	}
	add(&secret, credentialStoreRoots()...)
	return protect, secret
}
