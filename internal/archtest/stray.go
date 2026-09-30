package archtest

import (
	"fmt"
	"go/ast"
	"path"
	"sort"
	"strings"
)

// Stray output (docs/design/logging-v2.md §8.2): serve and the TUI run the
// code of cmd/tenant and every internal package, so a write to stderr or
// through the log package there bypasses the log book, and in the TUI draws
// over the screen. strayAllowed is the ratchet: the calls each file had when
// the rule came in (the one-shot commands' own terminal output, mostly). A
// count only goes down; a file not listed may have none.
var strayAllowed = map[string]int{
	"cmd/tenant/atlassianctl.go":    1,
	"cmd/tenant/attach.go":          2,
	"cmd/tenant/commands.go":        4,
	"cmd/tenant/eval.go":            5,
	"cmd/tenant/feedback.go":        1,
	"cmd/tenant/goalctl.go":         4,
	"cmd/tenant/gstackseeds.go":     2,
	"cmd/tenant/main.go":            2,
	"cmd/tenant/mcpctl.go":          5,
	"cmd/tenant/modelctl.go":        1,
	"cmd/tenant/peerctl.go":         16,
	"cmd/tenant/research.go":        4,
	"cmd/tenant/setup.go":           13,
	"cmd/tenant/setup_oauth.go":     21,
	"cmd/tenant/skillctl_gsuite.go": 1,
	"cmd/tenant/skills_setup.go":    9,
	"cmd/tenant/wiring.go":          1,
	"internal/plugins/x/auth.go":    1,
}

// longRunning reports whether file belongs to code serve and the TUI run.
func longRunning(file string) bool {
	dir := path.Dir(file)
	return match("cmd/**", dir) || match("internal/**", dir)
}

// strayCalls counts, per file of cmd/** and internal/**, the calls that
// write around the log book: fmt.Fprint* to os.Stderr, and the log package's
// Print*, Fatal* and Panic*.
//
// ponytail: syntax only, like countSites. A writer variable that holds
// os.Stderr is not seen; load go/types if one slips through.
func strayCalls(files []srcFile) map[string]int {
	out := map[string]int{}
	for _, f := range files {
		if !longRunning(f.rel) {
			continue
		}
		imports := importNames(f.ast)
		pkgOf := func(e ast.Expr) string {
			if id, ok := e.(*ast.Ident); ok {
				return imports[id.Name]
			}
			return ""
		}
		n := 0
		ast.Inspect(f.ast, func(nd ast.Node) bool {
			call, ok := nd.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch name := sel.Sel.Name; pkgOf(sel.X) {
			case "fmt":
				if strings.HasPrefix(name, "Fprint") && len(call.Args) > 0 {
					if w, ok := call.Args[0].(*ast.SelectorExpr); ok && w.Sel.Name == "Stderr" && pkgOf(w.X) == "os" {
						n++
					}
				}
			case "log":
				if strings.HasPrefix(name, "Print") || strings.HasPrefix(name, "Fatal") || strings.HasPrefix(name, "Panic") {
					n++
				}
			}
			return true
		})
		if n > 0 {
			out[f.rel] = n
		}
	}
	return out
}

// unconfiguredServers names the files that build more http.Servers than they
// hand to httplimits.Configure, which gives a server its limits and points its
// error log (TLS handshake failures, handler panics) at the log book instead
// of stderr.
func unconfiguredServers(files []srcFile, mod string) []string {
	var out []string
	for _, f := range files {
		if !longRunning(f.rel) {
			continue
		}
		imports := importNames(f.ast)
		is := func(e ast.Expr, pkg, name string) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != name {
				return false
			}
			id, ok := sel.X.(*ast.Ident)
			return ok && imports[id.Name] == pkg
		}
		servers, configured := 0, 0
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if is(n.Type, "net/http", "Server") {
					servers++
				}
			case *ast.CallExpr:
				if is(n.Fun, mod+"/internal/httplimits", "Configure") {
					configured++
				}
			}
			return true
		})
		if servers > configured {
			out = append(out, f.rel)
		}
	}
	return out
}

// strayRatchet holds each file to its allowed count of stray calls.
func strayRatchet(allowed, got map[string]int) []string {
	names := make([]string, 0, len(allowed)+len(got))
	for f := range allowed {
		names = append(names, f)
	}
	for f := range got {
		if _, ok := allowed[f]; !ok {
			names = append(names, f)
		}
	}
	sort.Strings(names)
	var out []string
	for _, f := range names {
		switch n, max := got[f], allowed[f]; {
		case n > max:
			out = append(out, fmt.Sprintf("%s writes to stderr or through the log package %d times, more than its %d: serve and the TUI own the terminal. Log through logs.For or slog.Default instead (docs/LOGGING.md)", f, n, max))
		case n < max:
			out = append(out, fmt.Sprintf("%s now has %d stray stderr or log package calls; lower its limit in strayAllowed, internal/archtest/stray.go, from %d to %d", f, n, max, n))
		}
	}
	return out
}
