package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A fleet machine is confirmed by the operator, at the terminal app or the
// command line (the dashboard's page comes with TEN-533), and by nothing
// else: no tool, no relay, no approval card. That is a property of who calls
// what, so it is checked on the source of the whole repository: a new path to
// Store.Confirm fails here until it is looked at.
func TestOnlyTheOperatorConfirmsAMachine(t *testing.T) {
	type site struct{ file, fn string }
	var (
		storeConfirms []site // calls of the store's Confirm (three arguments, the first not a context)
		opConfirms    []site // calls of fleetOps.confirmMachine
		tuiConfirms   []site // calls of FleetControl.Confirm in the terminal app
		mentions      []string
		toolNames     = map[string]string{}
	)
	// Where a relay, a tool or the dashboard lives: none of it may so much as
	// name the confirm operation.
	offLimits := regexp.MustCompile(`^(internal/(dashboard|plugins|toolmux|pluginreg|mcpserver|agent|orchestra)/|cmd/tenant/(discord|imessage|dashboard|plugin_|toolmux|mcp|cron|peer|extapprove|restricttools|team|research)[^/]*$|cmd/tenant/[^/]*(_dashctl|tools|_tool)\.go$)`)
	names := regexp.MustCompile(`\b(confirmMachine|fleetTUIControl|FleetControl|fleetConfirmCmd)\b`)
	fleetTool := regexp.MustCompile(`^fleet_[a-z_]+$`)

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
		importsFleet := slices.ContainsFunc(file.Imports, func(i *ast.ImportSpec) bool { return i.Path.Value == `"tenant/internal/fleet"` })
		if offLimits.MatchString(rel) {
			ast.Inspect(file, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && names.MatchString(id.Name) {
					mentions = append(mentions, rel+": "+id.Name)
				}
				return true
			})
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && fleetTool.MatchString(s) {
					toolNames[s] = rel
				}
			}
			return true
		})
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
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch {
				case sel.Sel.Name == "confirmMachine":
					opConfirms = append(opConfirms, site{rel, name})
				case sel.Sel.Name == "Confirm" && len(call.Args) == 3 && !isContext(call.Args[0]) && !strings.HasPrefix(rel, "internal/fleet/"):
					if strings.HasPrefix(rel, "internal/tui/") {
						tuiConfirms = append(tuiConfirms, site{rel, name})
					} else if importsFleet {
						storeConfirms = append(storeConfirms, site{rel, name})
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

	if want := []site{{"cmd/tenant/fleetctl.go", "fleetOps.confirmMachine"}}; !slices.Equal(storeConfirms, want) {
		t.Errorf("Store.Confirm is called from %v, want only %v", storeConfirms, want)
	}
	slices.SortFunc(opConfirms, func(a, b site) int { return strings.Compare(a.file+a.fn, b.file+b.fn) })
	if want := []site{{"cmd/tenant/fleetctl.go", "fleetConfirmCmd"}, {"cmd/tenant/fleettuictl.go", "fleetTUIControl.Confirm"}}; !slices.Equal(opConfirms, want) {
		t.Errorf("a machine is confirmed from %v, want only the command line and the terminal: %v", opConfirms, want)
	}
	if want := []site{{"internal/tui/cmd_fleet.go", "slashFleet"}}; !slices.Equal(tuiConfirms, want) {
		t.Errorf("the terminal app confirms a machine from %v, want only %v", tuiConfirms, want)
	}
	if len(mentions) > 0 {
		t.Errorf("a relay, a tool or the dashboard names the confirm operation:\n  %s", strings.Join(mentions, "\n  "))
	}
	// The fleet's tools are the spec's (section 4), and none of them enrolls,
	// confirms, renames, revokes or sets permissions. The other two names are
	// the settings file's keys.
	allowed := []string{"fleet_list", "fleet_exec", "fleet_process", "fleet_read", "fleet_list_dir", "fleet_write", "fleet_edit",
		"fleet_copy", "fleet_tools", "fleet_call", "fleet_note", "fleet_schemes", "fleet_rules"}
	for name, where := range toolNames {
		if !slices.Contains(allowed, name) {
			t.Errorf("%s names %q: the agent's fleet tools are %v, and an operator's act is not a tool", where, name, allowed[:11])
		}
	}
}

// isContext reports whether an argument reads as a context: an identifier or
// field with ctx in its name, or a call into the context package.
func isContext(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return strings.Contains(strings.ToLower(x.Name), "ctx")
	case *ast.SelectorExpr:
		return strings.Contains(strings.ToLower(x.Sel.Name), "ctx")
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok {
				return pkg.Name == "context"
			}
		}
	}
	return false
}

// A line a turn or a judge wrote never starts a slash command: the goal
// loop's next prompt is submitted by the terminal app, not typed, and it does
// not begin with "/" whatever the judge said.
func TestGoalPromptsAreNotCommands(t *testing.T) {
	gc := &goalControl{condition: "ship it"} // a goal is set
	for _, reason := range []string{"/fleet confirm web1 123456", "\n/fleet revoke web1 delete", ""} {
		if p := gc.Continue(reason); p == "" || strings.HasPrefix(strings.TrimSpace(p), "/") {
			t.Errorf("the goal loop would submit %q as a command", p)
		}
	}
}
