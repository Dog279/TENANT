package main

// fleetsetup.go is adding a machine to a fleet in one paste (TEN-541): the
// code made by fleetOps.issueCode, the lines to paste on the new machine
// (enrollPaste, the one place they are written, for `tenant fleet enroll`,
// /fleet enroll and the dashboard's Add machine), and the addresses a code
// carries (`tenant fleet address`), with a warning when none of them can be
// dialled from another site.
//
// A machine joins by the code and nothing else: no discovery, no request to
// join. The code pins the master before the machine sends anything, and the
// six digits it then shows are typed on the master, at this computer.

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"tenant/internal/fleet"
)

// enrolled is a code just made: what the operator is shown, once.
type enrolled struct {
	Name      string
	Code      string // a secret while it lives: shown, never logged or kept
	TTL       time.Duration
	Expires   time.Time
	Addresses []string // what the code tells the machine to dial
}

// issueCode makes the one-time code that enrolls one machine as name. by
// says who asked, for the Security record ("" at a terminal); the record
// names the machine and never holds the code. The fleet must exist already:
// the command line makes it first (initFleet), the terminal app and the
// dashboard tell the operator to.
func (o fleetOps) issueCode(name string, ttl time.Duration, adopt, by string) (enrolled, error) {
	id, err := fleetIdentity(o.cfgDir)
	if err != nil {
		return enrolled{}, err
	}
	addrs, err := o.store.Addresses()
	if err != nil {
		return enrolled{}, err
	}
	if len(addrs) == 0 {
		return enrolled{}, errors.New("the master has no address for a machine to dial yet: tenant fleet address add HOST:PORT")
	}
	if ttl == 0 {
		ttl = fleet.DefaultEnrollTTL
	}
	// Read before the code is made, so the expiry shown is never later than
	// the one the code carries.
	expires := time.Now().Add(ttl)
	code, err := o.store.CreateEnrollment(id.Fingerprint, name, ttl, adopt)
	if err != nil {
		return enrolled{}, err
	}
	detail := "valid for " + ttl.String()
	if by != "" {
		detail += "; asked by " + by
	}
	o.audit(fleet.Machine{Name: name, ID: adopt}, "code issued", detail)
	return enrolled{Name: name, Code: code, TTL: ttl, Expires: expires, Addresses: addrs}, nil
}

// pasteLine is one line to paste on the new machine, and where it runs.
type pasteLine struct{ For, Line string }

var (
	// codeShape is every character an enrollment code can hold: none of them
	// ends a quote, starts a variable or ends a line in a POSIX shell or in
	// PowerShell, so a code stands in single quotes in both, unchanged.
	codeShape = regexp.MustCompile(`^tntfleet1_[A-Za-z0-9_-]+$`)
	// repoShape is a GitHub owner/name, the only thing put into the scripts'
	// address.
	repoShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
)

// installScripts is where the install scripts are fetched from: the latest
// release of the repository `tenant update` takes releases from
// (updateRepo), "" for a build that names none.
func installScripts() string {
	r := updateRepo()
	if !repoShape.MatchString(r) || strings.Contains(r, "..") {
		return ""
	}
	return "https://github.com/" + r + "/releases/latest/download/"
}

// enrollPaste is the lines a new machine is given to join with code: as the
// user's own child from logon, or, with system, as a service that runs as
// root or SYSTEM from boot. Each is one line, and the code is in it as a
// variable's value or as standard input, never as an argument: the process
// list and the child's command line never show it. A machine with nothing on
// it gets install.sh or install.ps1, which go on to `tenant child install`;
// a machine that has Tenant gets that command alone. note says, in one
// sentence, why the first two are missing, "" when they are not.
func enrollPaste(code string, system bool) (lines []pasteLine, note string) {
	if len(code) > 8<<10 || !codeShape.MatchString(code) {
		return nil, "This is not an enrollment code, so there is nothing to paste."
	}
	q := "'" + code + "'"
	if base := installScripts(); base == "" {
		note = "This build of Tenant names no release repository, so it cannot print the line that installs Tenant too: install it on the new machine first, then paste a line below."
	} else if system {
		lines = append(lines,
			pasteLine{"Linux, macOS, nothing installed (sudo asks for your password)",
				"curl -fsSL " + base + "install.sh | TENANT_CHILD=system TENANT_ENROLL_CODE=" + q + " sudo --preserve-env=TENANT_CHILD,TENANT_ENROLL_CODE sh"},
			pasteLine{"Windows, nothing installed (PowerShell run as administrator)",
				"$env:TENANT_CHILD='system'; $env:TENANT_ENROLL_CODE=" + q + "; irm " + base + "install.ps1 | iex"})
	} else {
		lines = append(lines,
			pasteLine{"Linux, macOS, nothing installed",
				"curl -fsSL " + base + "install.sh | TENANT_CHILD=user TENANT_ENROLL_CODE=" + q + " sh"},
			pasteLine{"Windows, nothing installed (PowerShell)",
				"$env:TENANT_CHILD='user'; $env:TENANT_ENROLL_CODE=" + q + "; irm " + base + "install.ps1 | iex"})
	}
	if system {
		return append(lines,
			pasteLine{"Linux, macOS, Tenant installed (in a root shell)", "printf '%s\\n' " + q + " | tenant child install --system"},
			pasteLine{"Windows, Tenant installed (PowerShell run as administrator)", q + " | tenant child install --system"}), note
	}
	return append(lines,
		pasteLine{"Linux, macOS, Tenant installed", "printf '%s\\n' " + q + " | tenant child install"},
		pasteLine{"Windows, Tenant installed (PowerShell)", q + " | tenant child install"}), note
}

// addressWarning is the plain warning for a code whose every IP address is
// private, link-local or loopback, "" otherwise. A host name is not judged
// (where it leads depends on who asks), and a list of names alone gets no
// warning.
func addressWarning(addrs []string) string {
	var names []string
	ips := 0
	for _, a := range addrs {
		host, _, err := net.SplitHostPort(a)
		ip, perr := netip.ParseAddr(host)
		if err != nil || perr != nil {
			names = append(names, a)
			continue
		}
		if ip = ip.Unmap(); !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ""
		}
		ips++
	}
	if ips == 0 {
		return ""
	}
	w := "warning: every IP address this code carries is private, link-local or loopback: a machine at another site cannot dial these."
	if len(names) > 0 {
		w += " (" + strings.Join(names, ", ") + " is a name, and works from there only if it leads there to this computer.)"
	}
	return w + " Add one it can reach (an overlay network's address, or a public one): tenant fleet address add HOST:PORT"
}

// enrollText is what the operator reads under a new code, at the command line
// and in the terminal app: when it expires, the addresses it carries, every
// line to paste on the new machine, and the next step here.
func enrollText(e enrolled) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The code above enrolls one machine as %s. It works once and expires in %s (at %s).\n", fleetName(e.Name), e.TTL, fleetTime(e.Expires))
	fmt.Fprintf(&b, "The machine dials %s   (change: tenant fleet address add|remove HOST:PORT)\n", strings.Join(e.Addresses, ", "))
	if w := addressWarning(e.Addresses); w != "" {
		b.WriteString(w + "\n")
	}
	for _, system := range []bool{false, true} {
		lines, note := enrollPaste(e.Code, system)
		if system {
			b.WriteString("\nOr, as a service that runs as root or SYSTEM from boot:\n")
		} else {
			b.WriteString("\nOn the new machine, paste one line. The child runs as your account, from logon:\n")
		}
		if note != "" && !system {
			b.WriteString("  " + note + "\n")
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "  %s:\n    %s\n", l.For, l.Line)
		}
	}
	fmt.Fprintf(&b, "\nIt prints six digits and waits. Then, here:  tenant fleet confirm %s", fleetName(e.Name))
	return b.String()
}

// fleetAddressCmd is `tenant fleet address [list | add HOST:PORT | remove
// HOST:PORT]`: the addresses a code tells a new machine to dial, changed one
// at a time. Each change goes through the check `init --address` does
// (Store.SetAddresses), and a connected machine is told the new list over its
// link.
func fleetAddressCmd(args []string) error {
	cli, err := openFleetCLI(flag.NewFlagSet("fleet address", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if _, err := fleetIdentity(cli.c.cfgDir); err != nil {
		return err
	}
	have, err := cli.store.Addresses()
	if err != nil {
		return err
	}
	usage := errors.New("usage: tenant fleet address [list | add HOST:PORT | remove HOST:PORT]")
	verb := "list"
	if len(cli.pos) > 0 {
		verb = cli.pos[0]
	}
	switch {
	case verb == "list" && len(cli.pos) <= 1:
	case verb == "add" && len(cli.pos) == 2:
		if slices.Contains(have, cli.pos[1]) {
			return fmt.Errorf("%s is on the list already", fleetQuote(cli.pos[1]))
		}
		have = append(have, cli.pos[1])
		if err := cli.store.SetAddresses(have); err != nil {
			return err
		}
	case (verb == "remove" || verb == "rm") && len(cli.pos) == 2:
		i := slices.Index(have, cli.pos[1])
		if i < 0 {
			return fmt.Errorf("%s is not on the list", fleetQuote(cli.pos[1]))
		}
		have = slices.Delete(have, i, i+1)
		if err := cli.store.SetAddresses(have); err != nil {
			return err
		}
	default:
		return usage
	}
	if len(have) == 0 {
		fmt.Println("A new machine is given no address, so no code can be made: tenant fleet address add HOST:PORT")
		return nil
	}
	fmt.Println("A new machine dials, in this order:")
	for _, a := range have {
		fmt.Println("  " + a)
	}
	if w := addressWarning(have); w != "" {
		fmt.Println(w)
	}
	return nil
}
