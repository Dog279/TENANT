package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tenant/internal/fleet"
)

// pasteCodes are codes of every shape CreateEnrollment can make, and the
// edges of it: a dash or an underscore first, nothing but them, the longest.
var pasteCodes = []string{
	"tntfleet1_eyJuIjoid2ViMSIsImEiOlsiMTAuMC4wLjE6OTc3NyJdfQ",
	"tntfleet1_-_-_-_",
	"tntfleet1_" + strings.Repeat("Az09-_", 1300),
}

// The paste lines, for one code, exactly; each a single line in which the
// code is a variable's value or standard input, never an argument.
func TestEnrollPaste(t *testing.T) {
	t.Setenv("TENANT_UPDATE_REPO", "acme/tenant")
	const code = "tntfleet1_abc-_Z9"
	user, note := enrollPaste(code, false)
	if note != "" {
		t.Errorf("a note with a repository: %q", note)
	}
	want := []pasteLine{
		{"Linux, macOS, nothing installed", "curl -fsSL https://github.com/acme/tenant/releases/latest/download/install.sh | TENANT_CHILD=user TENANT_ENROLL_CODE='tntfleet1_abc-_Z9' sh"},
		{"Windows, nothing installed (PowerShell)", "$env:TENANT_CHILD='user'; $env:TENANT_ENROLL_CODE='tntfleet1_abc-_Z9'; irm https://github.com/acme/tenant/releases/latest/download/install.ps1 | iex"},
		{"Linux, macOS, Tenant installed", "printf '%s\\n' 'tntfleet1_abc-_Z9' | tenant child install"},
		{"Windows, Tenant installed (PowerShell)", "'tntfleet1_abc-_Z9' | tenant child install"},
	}
	if !slices.Equal(user, want) {
		t.Errorf("as the user:\n got %q\nwant %q", user, want)
	}
	boot, _ := enrollPaste(code, true)
	want = []pasteLine{
		{"Linux, macOS, nothing installed (sudo asks for your password)", "curl -fsSL https://github.com/acme/tenant/releases/latest/download/install.sh | TENANT_CHILD=system TENANT_ENROLL_CODE='tntfleet1_abc-_Z9' sudo --preserve-env=TENANT_CHILD,TENANT_ENROLL_CODE sh"},
		{"Windows, nothing installed (PowerShell run as administrator)", "$env:TENANT_CHILD='system'; $env:TENANT_ENROLL_CODE='tntfleet1_abc-_Z9'; irm https://github.com/acme/tenant/releases/latest/download/install.ps1 | iex"},
		{"Linux, macOS, Tenant installed (in a root shell)", "printf '%s\\n' 'tntfleet1_abc-_Z9' | tenant child install --system"},
		{"Windows, Tenant installed (PowerShell run as administrator)", "'tntfleet1_abc-_Z9' | tenant child install --system"},
	}
	if !slices.Equal(boot, want) {
		t.Errorf("from boot:\n got %q\nwant %q", boot, want)
	}
	for _, l := range append(user, boot...) {
		if strings.ContainsAny(l.Line, "\r\n") || strings.Count(l.Line, code) != 1 {
			t.Errorf("%q is not one line with the code once", l.Line)
		}
		// Before the code: a variable being set, or printf/nothing feeding a pipe.
		if i := strings.Index(l.Line, "'"+code+"'"); !strings.HasSuffix(l.Line[:i], "TENANT_ENROLL_CODE=") && !strings.HasPrefix(l.Line, "'") &&
			!strings.HasPrefix(l.Line, "printf '%s\\n' ") || !strings.Contains(l.Line[i:], "|") && !strings.HasSuffix(l.Line, " sh") {
			t.Errorf("the code is not a value or standard input in %q", l.Line)
		}
	}
}

// A build that names no repository, or a repository that is not one, prints
// no line that fetches a script: it says so, and gives the lines for a
// machine that has Tenant. Something that is not a code gets no line at all.
func TestEnrollPasteWithoutARepository(t *testing.T) {
	for _, repo := range []string{"", "acme", "acme/tenant/x", "acme/tenant; rm -rf ~", "acme/te'nant", "../tenant", "acme/..", "-acme/tenant", "acme/ten ant"} {
		t.Setenv("TENANT_UPDATE_REPO", repo)
		// "" leaves version.Repo, which a test build leaves empty.
		lines, note := enrollPaste(pasteCodes[0], false)
		if !strings.Contains(note, "names no release repository") || len(lines) != 2 || !strings.Contains(lines[0].Line, "tenant child install") {
			t.Errorf("repository %q: %q, %q", repo, note, lines)
		}
	}
	t.Setenv("TENANT_UPDATE_REPO", "acme/tenant")
	for _, bad := range []string{"", "tnt1_abc", "tntfleet1_", "tntfleet1_a'b", "tntfleet1_a b", "tntfleet1_a\nb", "tntfleet1_a$b", "tntfleet1_" + strings.Repeat("a", 9000)} {
		if lines, note := enrollPaste(bad, true); lines != nil || note == "" {
			t.Errorf("%q gave %q", bad, lines)
		}
	}
}

// The warning comes when every IP address a code carries is private,
// link-local or loopback; a name is not judged; one public address is enough.
func TestAddressWarning(t *testing.T) {
	for _, tc := range []struct {
		addrs []string
		warn  bool
		name  bool // the warning names the names it did not judge
	}{
		{[]string{"192.168.1.5:9777"}, true, false},
		{[]string{"10.0.0.2:9777", "172.16.0.9:9777", "127.0.0.1:9777", "[fe80::1]:9777", "169.254.3.4:9777", "[fd00::5]:9777", "[::1]:9777"}, true, false},
		{[]string{"[::ffff:192.168.1.5]:9777"}, true, false},
		{[]string{"DESKTOP-1:9777", "192.168.1.5:9777"}, true, true},
		{[]string{"DESKTOP-1:9777"}, false, false},
		{[]string{"master.example.net:9777"}, false, false},
		{[]string{"192.168.1.5:9777", "203.0.113.9:9777"}, false, false},
		{[]string{"100.101.102.103:9777"}, false, false}, // a tailnet address: an overlay, reachable from another site
		{[]string{"[2001:db8::1]:9777", "10.0.0.1:9777"}, false, false},
		{nil, false, false},
	} {
		w := addressWarning(tc.addrs)
		if (w != "") != tc.warn || tc.warn && !strings.Contains(w, "a machine at another site cannot dial these") ||
			tc.warn && !strings.Contains(w, "tenant fleet address add HOST:PORT") || strings.Contains(w, "DESKTOP-1:9777") != tc.name {
			t.Errorf("%v: %q", tc.addrs, w)
		}
	}
}

// `tenant fleet address` changes the list one address at a time, through the
// check `init --address` makes, and says when the list would leave a machine
// at another site nothing to dial.
func TestFleetAddressCommand(t *testing.T) {
	l := newFleetLab(t)
	if _, err := l.run("address"); err == nil || !strings.Contains(err.Error(), "no fleet yet") {
		t.Fatalf("address before init: %v", err)
	}
	l.must("init", "--listen", "127.0.0.1:0", "--address", "192.168.1.5:9777")
	out := l.must("address")
	if !strings.Contains(out, "  192.168.1.5:9777") || !strings.Contains(out, "cannot dial these") {
		t.Errorf("list:\n%s", out)
	}
	out = l.must("address", "add", "vpn.example.net:9777")
	if !strings.Contains(out, "  192.168.1.5:9777\n  vpn.example.net:9777") || !strings.Contains(out, "vpn.example.net:9777 is a name") {
		t.Errorf("add:\n%s", out)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"address", "add", "vpn.example.net:9777"}, "on the list already"},
		{[]string{"address", "add", "no-port"}, "not a host:port address"},
		{[]string{"address", "add", "host:99999"}, "not a host:port address"},
		{[]string{"address", "add", "\x1b[2Jhost:1"}, "not a host:port address"},
		{[]string{"address", "remove", "10.9.9.9:1"}, "not on the list"},
		{[]string{"address", "add"}, "usage"},
		{[]string{"address", "bogus", "a:1"}, "usage"},
	} {
		if _, err := l.run(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.args, err, tc.want)
		}
	}
	if addrs, _ := fleet.Open(l.cfg).Addresses(); !slices.Equal(addrs, []string{"192.168.1.5:9777", "vpn.example.net:9777"}) {
		t.Errorf("after the refusals: %v", addrs)
	}
	if out := l.must("address", "remove", "192.168.1.5:9777"); strings.Contains(out, "192.168.1.5") || strings.Contains(out, "cannot dial these") {
		t.Errorf("remove:\n%s", out)
	}
	out = l.must("address", "rm", "vpn.example.net:9777")
	if !strings.Contains(out, "no code can be made") {
		t.Errorf("an empty list:\n%s", out)
	}
	if _, err := l.run("enroll", "web1"); err == nil || !strings.Contains(err.Error(), "tenant fleet address add") {
		t.Errorf("enroll with no address: %v", err)
	}
}

// enroll prints every line, the addresses with the warning, and the expiry;
// the terminal app's /fleet enroll says the same through the same code.
func TestFleetEnrollPrintsEveryLine(t *testing.T) {
	t.Setenv("TENANT_UPDATE_REPO", "acme/tenant")
	l := newFleetLab(t)
	l.must("init", "--listen", "127.0.0.1:0", "--address", "10.0.0.1:9777")
	var code string
	stderr := captureStderr(t, func() { code = strings.TrimSpace(l.must("enroll", "web1")) })
	for _, system := range []bool{false, true} {
		lines, _ := enrollPaste(code, system)
		for _, ln := range lines {
			if !strings.Contains(stderr, "    "+ln.Line+"\n") {
				t.Errorf("the command does not print %q:\n%s", ln.Line, stderr)
			}
		}
	}
	for _, want := range []string{"expires in 15m0s (at ", "The machine dials 10.0.0.1:9777", "cannot dial these", "as root or SYSTEM from boot", "tenant fleet confirm web1"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the command does not say %q:\n%s", want, stderr)
		}
	}

	tui := newFleetTUIControl(l.c, l.saved(), &l.stgMu)
	got, err := tui.Enroll("web2")
	if err != nil {
		t.Fatal(err)
	}
	tcode, rest, _ := strings.Cut(got, "\n\n")
	lines, _ := enrollPaste(tcode, true)
	if !codeShape.MatchString(tcode) || tcode == code || !strings.Contains(rest, lines[0].Line) || !strings.Contains(rest, "/fleet confirm web2") || strings.Contains(rest, "tenant fleet confirm") {
		t.Errorf("/fleet enroll:\n%s", got)
	}
	if _, err := tui.Enroll("web 2"); err == nil || !strings.Contains(err.Error(), "not a machine name") {
		t.Errorf("/fleet enroll with a name that is not one: %v", err)
	}
	lone := newFleetLab(t)
	if _, err := newFleetTUIControl(lone.c, &settings{}, &lone.stgMu).Enroll("web1"); err == nil || !strings.Contains(err.Error(), "no fleet yet: /fleet init") {
		t.Errorf("/fleet enroll without a fleet: %v", err)
	}
}

// Making a code is the operator's act: the store's CreateEnrollment is called
// by fleetOps.issueCode alone, and that by the command line, the terminal
// app's control and the dashboard's, each from one place; no tool, relay or
// other dashboard file names it. (The fleet tools the agent is offered are
// pinned by TestOnlyTheOperatorConfirmsAMachine: none of them enrolls.)
func TestOnlyTheOperatorMakesACode(t *testing.T) {
	type site struct{ file, fn string }
	calls := map[string][]site{} // selector name -> where it is called
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						switch s := sel.Sel.Name; {
						case s == "CreateEnrollment", s == "issueCode", s == "Enroll" && len(call.Args) == 3 && !strings.HasPrefix(rel, "internal/fleet/"),
							s == "Enroll" && len(call.Args) == 1 && strings.HasPrefix(rel, "internal/tui/"):
							calls[s] = append(calls[s], site{rel, name})
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sorted := func(s []site) []site {
		slices.SortFunc(s, func(a, b site) int { return strings.Compare(a.file+a.fn, b.file+b.fn) })
		return s
	}
	if want := []site{{"cmd/tenant/fleetsetup.go", "fleetOps.issueCode"}}; !slices.Equal(calls["CreateEnrollment"], want) {
		t.Errorf("Store.CreateEnrollment is called from %v, want only %v", calls["CreateEnrollment"], want)
	}
	if want := []site{{"cmd/tenant/fleet_dashctl.go", "dashFleet.Enroll"}, {"cmd/tenant/fleetctl.go", "fleetEnrollCmd"}, {"cmd/tenant/fleettuictl.go", "fleetTUIControl.Enroll"}}; !slices.Equal(sorted(calls["issueCode"]), want) {
		t.Errorf("a code is made from %v, want only the command line, the terminal and the dashboard's Fleet page: %v", calls["issueCode"], want)
	}
	// FleetControl.Enroll (three arguments) in the dashboard, and the
	// terminal's (one) in /fleet.
	var dash, tui []site
	for _, s := range calls["Enroll"] {
		if strings.HasPrefix(s.file, "internal/tui/") {
			tui = append(tui, s)
		} else {
			dash = append(dash, s)
		}
	}
	if want := []site{{"internal/dashboard/fleet.go", "Server.handleFleetAdd"}}; !slices.Equal(dash, want) {
		t.Errorf("the dashboard makes a code from %v, want only %v", dash, want)
	}
	if want := []site{{"internal/tui/cmd_fleet.go", "slashFleet"}}; !slices.Equal(tui, want) {
		t.Errorf("the terminal app makes a code from %v, want only %v", tui, want)
	}
}
