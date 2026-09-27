package main

import (
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/osys"
)

// osPlugin declares the OS plugin: machine info, the file tools and gated
// shell exec. It needs no config, so the launch path always registers it
// (enabled only with --os); it has no stub.
func osPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:      "os",
		Title:   "OS access (sysinfo, files, gated shell)",
		Version: 1,
		Tools:   func() []model.ToolSpec { return osys.NewDispatcher(nil, osys.Policy{}).Tools() },
		Configure: func() skillKind {
			return skillKind{ID: "os", Label: "OS access (sysinfo, files, gated shell)", Wired: true}
		},
		SetupOnly: true,
		Setup:     9,
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
