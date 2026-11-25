package tui

// Fleet commands: /fleet, and each machine's scheme under /permissions fleet.

import (
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/cmdreg"
)

// FleetControl is the operator's side of a master's fleet (TEN-531), over
// the same operations as `tenant fleet`. What it returns is ready to show:
// whatever a machine said about itself is already escaped and marked as the
// machine's own words.
type FleetControl interface {
	// Init makes this Tenant a fleet master if it is not one, as `tenant fleet
	// init` does: listen is where the fleet port binds ("" keeps what is set),
	// addresses what its machines dial (none keeps the list, or guesses a first
	// one). It does everything but the recovery key, which is FleetInit's.
	Init(listen string, addresses []string) (FleetInit, error)
	// Status lists the machines, each online or offline when this process
	// holds the fleet, and says which process holds it.
	Status() string
	// Show is everything known about one machine.
	Show(name string) (string, error)
	// Confirm with no code is the confirm screen for a pending machine: what it
	// reported and the approval scheme it would start with. With the six digits
	// the machine shows it confirms the machine; set is category=mode pairs for
	// that machine alone.
	Confirm(name, code string, set []string) (string, error)
	// Enroll makes the one-time code that adds a machine as name, and says
	// what `tenant fleet enroll` says: the code, the lines to paste on the new
	// machine, the addresses the code carries and the next step.
	Enroll(name string) (string, error)
	// Revoke ends a machine. scheme is "keep" or "delete" (its approval
	// scheme); "" only asks which.
	Revoke(name, scheme string) (string, error)
	// Perms is one machine's own approval scheme, for /permissions fleet <name>.
	Perms(name string) (PermissionControl, error)
	// Defaults are the fleet's rules for new machines.
	Defaults() ToolRuleControl
	// Relay shows or sets whether a relay's turns may reach the fleet: args is
	// nothing, a relay, or a relay and on|off.
	Relay(args []string) (string, error)
	// RemoteConfirm shows or sets whether a signed-in dashboard browser on
	// another computer may confirm a machine, with the sign-in key again:
	// args is nothing, or on|off.
	RemoteConfirm(args []string) (string, error)
}

// FleetInit is an init that has done everything but the recovery key.
type FleetInit struct {
	// Recovery makes or proves the fleet's recovery key; nil when it is proven
	// already. It runs on the terminal itself, with the app out of the way
	// (tea.Exec): the recovery identity is shown there once and typed back, and
	// never passes through the app, its transcript or a log.
	Recovery func() error
	// Done says what init did, once Recovery has run; recoveryErr is its error.
	Done func(recoveryErr error) string
}

// fleetInitMsg carries /fleet init from one step to the next: ran says the
// recovery step has run, with err.
type fleetInitMsg struct {
	FleetInit
	ran bool
	err error
}

// next is the rest of /fleet init: the recovery step with the terminal handed
// to it, then what init says.
func (in fleetInitMsg) next() tea.Cmd {
	if in.Recovery != nil && !in.ran {
		return tea.Exec(terminalStep(in.Recovery), func(err error) tea.Msg { return fleetInitMsg{in.FleetInit, true, err} })
	}
	return func() tea.Msg { return sysChatMsg{text: in.Done(in.err)} }
}

// terminalStep is a function as a tea.ExecCommand: it runs while the app has
// released the terminal, on the process's own standard input and output.
type terminalStep func() error

func (s terminalStep) Run() error        { return s() }
func (terminalStep) SetStdin(io.Reader)  {}
func (terminalStep) SetStdout(io.Writer) {}
func (terminalStep) SetStderr(io.Writer) {}

// fleetInitArgs reads /fleet init's flags, as `tenant fleet init` takes them:
// --listen ADDR once, --address HOST:PORT any number of times.
func fleetInitArgs(f []string) (listen string, addresses []string, ok bool) {
	for i := 0; i < len(f); i++ {
		name, val, eq := strings.Cut(f[i], "=")
		if !eq {
			if i++; i == len(f) {
				return "", nil, false
			}
			val = f[i]
		}
		switch name {
		case "--listen", "-listen":
			listen = val
		case "--address", "-address":
			addresses = append(addresses, val)
		default:
			return "", nil, false
		}
	}
	return listen, addresses, true
}

// CronFleetControl is the part of a CronControl that opens a job for fleet
// machines (/cron fleet). A job reaches none until the operator names them.
type CronFleetControl interface {
	SetFleet(id string, machines []string) (CronJobView, error)
}

const fleetUsage = "usage: /fleet   |   /fleet init [--listen ADDR] [--address HOST:PORT]...   |   /fleet show <name>   |   /fleet enroll <name>   |   /fleet confirm <name> [<six digits> [category=mode ...]]   |   /fleet revoke <name> [keep|delete]   |   /fleet relay [discord|imessage [on|off]]   |   /fleet remote-confirm [on|off]   (a machine's permissions: /permissions fleet <name>)"

// slashFleet runs /fleet.
func slashFleet(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	fc := m.cfg.Fleet
	if fc == nil {
		m.sysChat("the fleet is not available in this session")
		return nil
	}
	f := strings.Fields(arg)
	sub := ""
	if len(f) > 0 {
		sub = strings.ToLower(f[0])
	}
	// Each runs off the UI goroutine: it reads and writes fleet.json under a
	// lock other processes take.
	answer := func(run func() (string, error)) tea.Cmd {
		return func() tea.Msg {
			text, err := run()
			if err != nil {
				return sysChatMsg{text: "fleet: " + err.Error()}
			}
			return sysChatMsg{text: text}
		}
	}
	// Changing the fleet is the operator's act at this keyboard. A line the app
	// submitted by itself (a goal's next prompt) only gets to look.
	typed := func() bool {
		if !m.typed {
			m.sysChat("fleet: type this command here yourself; a line the app submits does not change the fleet")
		}
		return m.typed
	}
	switch {
	case sub == "" || sub == "list" || sub == "status":
		return answer(func() (string, error) { return fc.Status(), nil })
	case sub == "init":
		listen, addresses, ok := fleetInitArgs(f[1:])
		if !ok {
			break
		}
		if !typed() {
			return nil
		}
		return func() tea.Msg {
			in, err := fc.Init(listen, addresses)
			if err != nil {
				return sysChatMsg{text: "fleet: " + err.Error()}
			}
			return fleetInitMsg{FleetInit: in}
		}
	case sub == "show" && len(f) == 2:
		return answer(func() (string, error) { return fc.Show(f[1]) })
	case sub == "enroll" && len(f) == 2:
		if !typed() {
			return nil
		}
		return answer(func() (string, error) { return fc.Enroll(f[1]) })
	case sub == "confirm" && len(f) >= 2:
		// The six digits may be typed with a space in them; a pair has an "=".
		var code string
		var set []string
		for _, tok := range f[2:] {
			if strings.Contains(tok, "=") {
				set = append(set, tok)
			} else {
				code += tok
			}
		}
		if code != "" && !typed() {
			return nil
		}
		return answer(func() (string, error) { return fc.Confirm(f[1], code, set) })
	case sub == "revoke" && (len(f) == 2 || len(f) == 3):
		scheme := ""
		if len(f) == 3 {
			if scheme = strings.ToLower(f[2]); !typed() {
				return nil
			}
		}
		return answer(func() (string, error) { return fc.Revoke(f[1], scheme) })
	case sub == "relay" && len(f) <= 3:
		if len(f) == 3 && !typed() {
			return nil
		}
		return answer(func() (string, error) { return fc.Relay(f[1:]) })
	case sub == "remote-confirm" && len(f) <= 2:
		if len(f) == 2 && !typed() {
			return nil
		}
		return answer(func() (string, error) { return fc.RemoteConfirm(f[1:]) })
	}
	m.sysChat(fleetUsage)
	return nil
}

// rulesOnly is a rule set with no categories of its own, as the permissions
// editor shows the fleet's rules for new machines.
type rulesOnly struct{ ToolRuleControl }

func (rulesOnly) Permissions() []PermissionInfo              { return nil }
func (rulesOnly) SetPermission(string, string) (bool, error) { return false, nil }

// fleetPermissions is /permissions fleet …: one machine's scheme, or with
// --defaults the fleet's rules for new machines, in the same editor and the
// same words as the master's own.
func (m *model) fleetPermissions(f []string) {
	const usage = "usage: /permissions fleet <name> [set <category> <mode> | add <pattern> <mode> | rm <pattern>]   |   /permissions fleet --defaults [add <pattern> <mode> | rm <pattern>]"
	if m.cfg.Fleet == nil {
		m.sysChat("the fleet is not available in this session")
		return
	}
	if len(f) == 0 {
		m.sysChat(usage)
		return
	}
	var pc PermissionControl
	header, cmd := "Fleet rules: what every newly confirmed machine starts with", "/permissions fleet --defaults"
	if f[0] == "--defaults" {
		pc = rulesOnly{m.cfg.Fleet.Defaults()}
	} else {
		var err error
		if pc, err = m.cfg.Fleet.Perms(f[0]); err != nil {
			m.sysChat("fleet: " + err.Error())
			return
		}
		header, cmd = "Permissions of fleet machine "+f[0]+" (this machine only)", "/permissions fleet "+f[0]
	}
	m.editPermissions(pc, header, cmd, usage, f[1:])
}

// fleetCommands are the fleet's commands.
func fleetCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/fleet"}, Section: "fleet", Run: slashFleet,
			Summary: "the machines this Tenant is the master of: set up, status, confirm, revoke",
			Rows: []cmdreg.Row{
				{Syntax: "/fleet", Desc: "each machine: online or offline, OS, user, last seen; and which process holds the fleet"},
				{Syntax: "/fleet init [--listen ADDR] [--address HOST:PORT]...", Desc: "make this Tenant a fleet master (once); the app steps aside while the recovery identity is shown on the terminal and typed back"},
				{Syntax: "/fleet show <name>", Desc: "everything known about one machine, with what it reported marked as its own words"},
				{Syntax: "/fleet enroll <name>", Desc: "a one-time code for a new machine, and the line to paste there, with Tenant on it or with nothing on it"},
				{Syntax: "/fleet confirm <name>", Desc: "the confirm screen of a pending machine: what it reported, the scheme it starts with"},
				{Syntax: "/fleet confirm <name> <six digits> [category=mode ...]", Desc: "confirm it with the digits it shows; a pair sets that category for this machine alone"},
				{Syntax: "/fleet revoke <name> [keep|delete]", Desc: "end a machine; keep or delete its approval scheme"},
				{Syntax: "/fleet relay [discord|imessage [on|off]]", Desc: "whether a relay's turns may reach the fleet at all (default off); a machine's permissions: /permissions fleet <name>"},
				{Syntax: "/fleet remote-confirm [on|off]", Desc: "whether a signed-in dashboard browser on another computer may confirm a machine, with the sign-in key again (default off)"},
			}},
	}
}
