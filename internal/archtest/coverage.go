package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Catalog coverage (docs/design/logging-v2.md §13, phase 4): the share of
// each provider's static warning-and-above logging call sites that carry a
// catalogued event ID, and a ratchet that keeps a catalogued package from
// adding call sites without one.

// providers maps each provider to the code whose call sites count toward
// it: rules' package patterns, or a file glob for files of the composition
// root. The first entry that matches a file governs it, so the specific ones
// come first; system takes the rest of cmd/tenant.
var providers = []struct {
	Name  string
	Paths []string
}{
	{"security", []string{"internal/approval", "cmd/tenant/approval.go", "cmd/tenant/extapprove.go", "cmd/tenant/securityaudit.go", "cmd/tenant/permissionsctl.go"}},
	{"turns", []string{"internal/agent", "cmd/tenant/turngate.go"}},
	{"subagents", []string{"internal/orchestra", "cmd/tenant/team.go", "cmd/tenant/builtinagents.go"}},
	{"research", []string{"internal/research", "cmd/tenant/research.go"}},
	{"discord", []string{"internal/plugins/discord", "cmd/tenant/discord*.go", "cmd/tenant/plugin_discord.go"}},
	{"imessage", []string{"internal/plugins/imessage", "cmd/tenant/imessage*.go", "cmd/tenant/plugin_imessage.go"}},
	{"cron", []string{"internal/cron", "internal/plugins/cron", "cmd/tenant/cron*.go"}},
	{"mcp.*", []string{"internal/mcp/**", "internal/mcpserver", "internal/plugins/mcpremote", "cmd/tenant/mcp*.go"}},
	{"sandbox", []string{"internal/sandbox/**", "cmd/tenant/sandbox*.go", "cmd/tenant/execprofiles.go"}},
	{"model", []string{"internal/model/**", "cmd/tenant/model*.go", "cmd/tenant/fallback.go"}},
	{"route", []string{"internal/route", "cmd/tenant/route*.go"}},
	{"skills", []string{"internal/skillpkg", "internal/memory/skills", "cmd/tenant/skill*.go"}},
	{"memory", []string{"internal/memory/**", "cmd/tenant/memory*.go", "cmd/tenant/recall_tool.go"}},
	{"improve", []string{"internal/improve", "cmd/tenant/improveprofile.go", "cmd/tenant/selfimprove_feed.go", "cmd/tenant/souljob_wire.go"}},
	{"dashboard", []string{"internal/dashboard", "cmd/tenant/dashboard*.go"}},
	{"peering", []string{"internal/peering", "cmd/tenant/peer*.go"}},
	{"plugin.*", []string{"internal/plugins/*", "internal/toolmux", "internal/pluginreg", "cmd/tenant/plugin_*.go"}},
	{"backup", []string{"internal/backup", "cmd/tenant/backupctl.go"}},
	{"privacy", []string{"internal/privacy", "cmd/tenant/privacy*.go"}},
	{"eval", []string{"internal/eval/**", "cmd/tenant/eval*.go", "cmd/tenant/judgectl.go"}},
	{"logbook", []string{"internal/logs/**", "cmd/tenant/logctl.go", "cmd/tenant/attach.go"}},
	{"system", []string{"cmd/tenant", "internal/tui", "internal/lifecycle", "internal/config", "internal/secrets", "internal/fsperm",
		"internal/datalock", "internal/version", "internal/cmdreg", "internal/control", "internal/httplimits", "internal/runtrace"}},
}

// catalogued is the ratchet. A package listed here is catalogued: it may
// have at most this many warning-or-error call sites without an event ID.
// A coverage ticket adds its packages with their counts. A count only goes
// down: give a new call site a catalogued event, and lower the number when
// the test says so.
var catalogued = map[string]int{
	"internal/plugins/discord":  0,
	"internal/plugins/imessage": 0,
	"internal/model":            0, "internal/model/backend/anthropic": 0, "internal/model/backend/claudecode": 0,
	"internal/model/backend/echo": 0, "internal/model/backend/vllm": 0, "internal/model/toolfmt": 0,
	"internal/route": 0,
	"internal/cron":  0, "internal/plugins/cron": 0,
	"internal/peering": 0,
	// plugin.* (TEN-393)
	"internal/pluginreg": 0, "internal/toolmux": 0,
	"internal/plugins/atlassian": 0, "internal/plugins/crm": 0, "internal/plugins/github": 0,
	"internal/plugins/gsuite": 0, "internal/plugins/osys": 0, "internal/plugins/simplefin": 0,
	"internal/plugins/sql": 0, "internal/plugins/wiki": 0, "internal/plugins/x": 0,
	"internal/plugins/web": 1, // cfg.Warn, the NoSandbox callback a host may install
	// memory and improve (TEN-390)
	"internal/improve":            0,
	"internal/memory":             0,
	"internal/memory/archive":     0,
	"internal/memory/assemble":    0,
	"internal/memory/compress":    0,
	"internal/memory/cosine":      0,
	"internal/memory/distill":     0,
	"internal/memory/episodic":    0,
	"internal/memory/ftsutil":     0,
	"internal/memory/mdimport":    0,
	"internal/memory/semantic":    0,
	"internal/memory/sme":         0,
	"internal/memory/soul":        0,
	"internal/memory/usage":       0,
	"internal/memory/userprofile": 0,
	"internal/memory/working":     0,
	// backup, privacy and skills (TEN-394)
	"internal/backup":        0,
	"internal/privacy":       0,
	"internal/memory/skills": 0,
	"internal/skillpkg":      0,
	"internal/agent":         0, "internal/orchestra": 0, "internal/research": 0,
	// system (TEN-383); cmd/tenant is shared with the providers still to come
	"internal/lifecycle": 0, "internal/tui": 0, "internal/config": 0, "internal/secrets": 0,
	"internal/fsperm": 0, "internal/datalock": 0, "internal/version": 0, "internal/cmdreg": 0,
	"internal/control": 0, "internal/httplimits": 0, "internal/runtrace": 0,
	// dashboard (TEN-391) and eval (TEN-395)
	"internal/dashboard": 0,
	"internal/eval":      0, "internal/eval/compaction": 0,
	// mcp.* (TEN-387) and sandbox (TEN-388)
	"internal/plugins/mcpremote": 0, "internal/mcpserver": 0, "internal/mcp": 0, "internal/mcp/transport": 0,
	"internal/sandbox": 0, "internal/sandbox/escapecore": 0, "internal/sandbox/escapetest": 0, "internal/sandbox/redteamllm": 0,
}

// providerOf names the provider file (module-relative) counts toward, or ""
// for code outside Tenant's runtime (reference, scripts, these tests).
func providerOf(file string) string {
	for _, p := range providers {
		for _, pat := range p.Paths {
			if strings.HasSuffix(pat, ".go") {
				if ok, _ := path.Match(pat, file); ok {
					return p.Name
				}
			} else if match(pat, path.Dir(file)) {
				return p.Name
			}
		}
	}
	return ""
}

// tally counts one file's static warning-and-above logging call sites.
type tally struct{ ID, NoID int }

// countSites parses the non-test Go files under root, which holds module mod,
// and tallies each file's (module-relative) warning-and-above call sites:
//   - with an ID: a Def's Log or Audit, when the Def is a package-level
//     variable declared at warning or worse (P.Event(id, event.Warning, …)
//     and the methods chained on it), or a struct field one is assigned to
//     (pluginreg.Events' shared plugin events); and event.Start, whose
//     activity fails at error;
//   - without: a Warn or Error (of a logger, logs.For's or package slog's), a
//     Log or LogAttrs at a constant level of warning or worse, and the hub's
//     Logf at one.
//
// ponytail: syntax only, no type checking. Any method called Warn or Error
// with arguments counts as a logger's, a Def held in a field is known by the
// field's name alone, and one held anywhere but a package-level variable or
// a field is not recognised. Load go/types if the figures drift from a
// reader's count.
func countSites(root, mod string) (map[string]tally, error) {
	files, err := parseTree(root)
	if err != nil {
		return nil, err
	}

	// Every Def declared at package level: "<package dir>.<name>" → whether
	// it is a warning or worse.
	defs := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.ast.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, v := range vs.Values {
						if warn, ok := defLevel(v); ok && i < len(vs.Names) {
							defs[path.Dir(f.rel)+"."+vs.Names[i].Name] = warn
						}
					}
				}
			}
		}
	}

	// Every Def assigned to a struct field, Events{Failed: P.Event(…)} or
	// e.Failed = P.Event(…): the field's name → whether a warning or worse.
	fields := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if k, isIdent := n.Key.(*ast.Ident); isIdent {
					if warn, ok := defLevel(n.Value); ok {
						fields[k.Name] = warn
					}
				}
			case *ast.AssignStmt:
				for i, l := range n.Lhs {
					if sel, isSel := l.(*ast.SelectorExpr); isSel && i < len(n.Rhs) {
						if warn, ok := defLevel(n.Rhs[i]); ok {
							fields[sel.Sel.Name] = warn
						}
					}
				}
			}
			return true
		})
	}

	out := make(map[string]tally, len(files))
	for _, f := range files {
		imports := importNames(f.ast)
		// def resolves x, a Def, pkg.Def or v.Field, to whether it is a
		// warning or worse.
		def := func(x ast.Expr) (warn, ok bool) {
			switch x := x.(type) {
			case *ast.Ident:
				warn, ok = defs[path.Dir(f.rel)+"."+x.Name]
			case *ast.SelectorExpr:
				if pkg, isIdent := x.X.(*ast.Ident); isIdent && imports[pkg.Name] != "" {
					warn, ok = defs[strings.TrimPrefix(imports[pkg.Name], mod+"/")+"."+x.Sel.Name]
				}
				if !ok {
					warn, ok = fields[x.Sel.Name]
				}
			}
			return warn, ok
		}
		var t tally
		ast.Inspect(f.ast, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg := ""
			if id, ok := sel.X.(*ast.Ident); ok {
				pkg = imports[id.Name]
			}
			args := call.Args
			switch name := sel.Sel.Name; {
			case pkg != "" && pkg != "log/slog": // a package's function, like http.Error
				if pkg == mod+"/internal/logs/event" && name == "Start" {
					t.ID++
				}
			case name == "Log" || name == "Audit":
				if warn, ok := def(sel.X); ok {
					if warn {
						t.ID++
					}
				} else if len(args) >= 2 && warnLevel(args[1]) {
					t.NoID++
				}
			case name == "LogAttrs" || name == "Logf":
				if len(args) >= 2 && warnLevel(args[1]) {
					t.NoID++
				}
			case name == "Warn" || name == "Error" || name == "WarnContext" || name == "ErrorContext":
				if len(args) > 0 { // err.Error() takes none
					t.NoID++
				}
			}
			return true
		})
		out[f.rel] = t
	}
	return out, nil
}

// srcFile is a parsed Go file and its path relative to the tree's root.
type srcFile struct {
	rel string
	ast *ast.File
}

// parseTree parses the non-test Go files under root, skipping testdata,
// vendor and hidden directories.
func parseTree(root string) ([]srcFile, error) {
	var files []srcFile
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		files = append(files, srcFile{filepath.ToSlash(rel), f})
		return err
	})
	return files, err
}

// importNames maps the name each import is used by in f to its path.
func importNames(f *ast.File) map[string]string {
	imports := map[string]string{}
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		name := path.Base(p)
		if im.Name != nil {
			name = im.Name.Name
		}
		imports[name] = p
	}
	return imports
}

// defLevel reports whether e declares a Def (P.Event(id, level, task,
// template, …) and the methods chained on it), and whether at warning or
// worse.
func defLevel(e ast.Expr) (warn, ok bool) {
	for {
		call, isCall := e.(*ast.CallExpr)
		if !isCall {
			return false, false
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return false, false
		}
		if sel.Sel.Name == "Event" && len(call.Args) >= 4 {
			return warnLevel(call.Args[1]), true
		}
		e = sel.X
	}
}

var warnNames = map[string]bool{"LevelWarn": true, "LevelError": true, "LevelCritical": true, "Warning": true, "Error": true, "Critical": true}

// warnLevel reports whether e is a constant level of warning or worse:
// slog.LevelWarn or LevelError, logs.LevelCritical, event.Warning, Error or
// Critical, or one of those plus an offset.
func warnLevel(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		return warnNames[e.Sel.Name]
	case *ast.Ident:
		return warnNames[e.Name]
	case *ast.BinaryExpr:
		return e.Op == token.ADD && warnLevel(e.X)
	case *ast.ParenExpr:
		return warnLevel(e.X)
	}
	return false
}

// byPackage sums tallies by package directory. Every package appears, even
// one with no call sites.
func byPackage(files map[string]tally) map[string]tally {
	out := map[string]tally{}
	for f, t := range files {
		s := out[path.Dir(f)]
		s.ID += t.ID
		s.NoID += t.NoID
		out[path.Dir(f)] = s
	}
	return out
}

// ratchet holds each catalogued package (limits) to its count of call sites
// without an ID, from pkgs.
func ratchet(limits map[string]int, pkgs map[string]tally) []string {
	names := make([]string, 0, len(limits))
	for p := range limits {
		names = append(names, p)
	}
	sort.Strings(names)
	var out []string
	for _, p := range names {
		t, ok := pkgs[p]
		switch max := limits[p]; {
		case !ok:
			out = append(out, fmt.Sprintf("%s is marked catalogued in internal/archtest/coverage.go, but there is no such package", p))
		case t.NoID > max:
			out = append(out, fmt.Sprintf("%s is catalogued: it has %d warning or error call sites without an event ID, more than its %d; log the new ones through a catalogued event (docs/LOGGING.md)", p, t.NoID, max))
		case t.NoID < max:
			out = append(out, fmt.Sprintf("%s now has %d warning or error call sites without an event ID; lower its limit in catalogued, internal/archtest/coverage.go, from %d to %d", p, t.NoID, max, t.NoID))
		}
	}
	return out
}
