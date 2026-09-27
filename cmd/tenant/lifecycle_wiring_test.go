package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// shutdownSteps parses fn in file and returns the stops it registers with the
// lifecycle group (step name → stage), plus any deferred call that is not a
// mutex unlock, the log hub's close, the signal context's stop or the
// group's own Shutdown.
func shutdownSteps(t *testing.T, file, fn string) (steps map[string]string, strayDefers []string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	steps = map[string]string{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || len(n.Args) < 2 {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "life" || (sel.Sel.Name != "Add" && sel.Sel.Name != "AddFunc") {
					return true
				}
				stage, ok := n.Args[0].(*ast.SelectorExpr)
				lit, ok2 := n.Args[1].(*ast.BasicLit)
				if !ok || !ok2 {
					t.Errorf("%s: a stop registered with a computed stage or name", fn)
					return true
				}
				name, _ := strconv.Unquote(lit.Value)
				steps[name] = stage.Sel.Name
			case *ast.DeferStmt:
				switch fun := n.Call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == "stop" { // the signal context's, released with the process
						return true
					}
					strayDefers = append(strayDefers, fun.Name)
					return true
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok {
						call := x.Name + "." + fun.Sel.Name
						if fun.Sel.Name == "Unlock" || call == "hub.Close" || call == "life.Shutdown" {
							return true
						}
						strayDefers = append(strayDefers, call)
						return true
					}
				}
				strayDefers = append(strayDefers, "a deferred call")
			}
			return true
		})
		return steps, strayDefers
	}
	t.Fatalf("%s has no func %s", file, fn)
	return nil, nil
}

// serve and the TUI stop the hub they share in the same stages: pending
// approvals are denied, then the listeners stop taking work, then in-flight
// work drains, then what it used is closed (TEN-194, TEN-314). The TUI used to
// skip the first two, so a background turn could arrive, or sit blocked on an
// approval, while its stores closed.
func TestServeAndTUITearDownAlike(t *testing.T) {
	shared := map[string]string{
		"pending approvals": "Unblock",
		"peer listener":     "Intake",
		"discord relay":     "Intake",
		"dashboard":         "Intake",
		"cron":              "Drain",
		"improve scheduler": "Drain",
		"stores":            "Release",
		"usage ledger":      "Release",
		"tool mux":          "Release",
		"skills store":      "Release",
		"improve meta":      "Release",
		"team bus":          "Release",
		"team runtime":      "Release",
		"tool stats":        "Release",
	}
	for _, cmd := range []struct{ file, fn string }{{"serve.go", "cmdServe"}, {"commands.go", "cmdTUI"}} {
		steps, stray := shutdownSteps(t, cmd.file, cmd.fn)
		for name, stage := range shared {
			if got, ok := steps[name]; !ok {
				t.Errorf("%s does not stop %q at shutdown", cmd.fn, name)
			} else if got != stage {
				t.Errorf("%s stops %q in stage %s, want %s", cmd.fn, name, got, stage)
			}
		}
		for _, d := range stray {
			t.Errorf("%s defers %s; register it with the lifecycle group so it runs in its stage", cmd.fn, d)
		}
	}
}
