package main

import (
	"context"
	"flag"
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/osys"
)

// osPlugin declares the OS plugin: machine info, the file tools and gated
// shell exec. It needs no config, so the launch path always registers it
// (enabled only with --os); it has no stub.
func osPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "os",
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
// (execprofiles.go): the turn's surface picks its exec profile per call, the
// default "host" profile is the pre-seam behaviour, and a sandbox refusal
// escalates through the same origin-routed confirm as every other gated
// action.
func launchOS(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	svc, err := osys.Open(osys.Config{Runner: sandboxRunner(e.c.cfgDir, e.c.dataDir, e.confirm)})
	if err != nil {
		return nil, nil, nil // no OS service here: the plugin is simply absent
	}
	f := osFlagsOf(e.pf)
	return []plugin{osys.NewDispatcher(svc, osys.Policy{
		AllowExec:  e.gateAllow(f.allowExec),
		AllowWrite: e.gateAllow(f.allowWrite),
		Confirm:    e.confirm,
		Roots:      resolveOSRoots(e.pf, e.c.cfgDir, e.c.dataDir, e.c.lc),
	})}, nil, nil
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
