package archtest

import (
	"bufio"
	"flag"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the rules table in docs/ARCHITECTURE.md")

const (
	docPath    = "docs/ARCHITECTURE.md"
	tableBegin = "<!-- archtest:rules -->"
	tableEnd   = "<!-- /archtest:rules -->"
)

// moduleRoot finds the directory holding go.mod, and the module path.
func moduleRoot(t *testing.T) (string, string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		f, err := os.Open(filepath.Join(dir, "go.mod"))
		if err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if mod, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
					return dir, strings.TrimSpace(mod)
				}
			}
			t.Fatalf("%s/go.mod has no module line", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// graph is the module's import graph from its non-test source: module
// packages (relative paths) to the module packages and the external import
// paths they import. Test files are left out, so an external test package
// may import more than its package does.
type graph struct {
	internal map[string][]string
	external map[string][]string
}

func loadGraph(t *testing.T) graph {
	t.Helper()
	root, mod := moduleRoot(t)
	g := graph{internal: map[string][]string{}, external: map[string][]string{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return err
		}
		for _, cg := range f.Comments {
			if cg.Pos() < f.Package && strings.Contains(cg.Text(), "go:build ignore") {
				return nil
			}
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(rel)
		if _, ok := g.internal[pkg]; !ok {
			g.internal[pkg] = nil
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if in, ok := strings.CutPrefix(p, mod+"/"); ok {
				g.internal[pkg] = appendUnique(g.internal[pkg], in)
			} else {
				g.external[pkg] = appendUnique(g.external[pkg], p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.internal) < 20 {
		t.Fatalf("found only %d packages under %s; the walk is broken", len(g.internal), root)
	}
	return g
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func TestDependencyDirection(t *testing.T) {
	for _, v := range violations(loadGraph(t).internal, rules) {
		t.Error(v)
	}
}

// A rule that matches no package is left over from a package that moved or
// went away.
func TestEveryRuleGovernsAPackage(t *testing.T) {
	g := loadGraph(t)
	for _, r := range rules {
		used := false
		for p := range g.internal {
			if got, ok := ruleFor(rules, p); ok && got.Pkg == r.Pkg {
				used = true
				break
			}
		}
		if !used {
			t.Errorf("rule %q governs no package; remove it or fix its pattern", r.Pkg)
		}
	}
}

// Bubble Tea is the terminal UI's business: the runtime, the dashboard and
// the stores must never pull it in.
func TestOnlyTheTerminalUIUsesBubbleTea(t *testing.T) {
	for _, v := range uiOnly(loadGraph(t).external) {
		t.Error(v)
	}
	got := uiOnly(map[string][]string{
		"internal/tui":       {"github.com/charmbracelet/bubbletea"},
		"cmd/tenant":         {"github.com/charmbracelet/lipgloss"},
		"internal/dashboard": {"github.com/charmbracelet/lipgloss", "golang.org/x/net"},
	})
	if len(got) != 1 || !strings.HasPrefix(got[0], "internal/dashboard imports github.com/charmbracelet/lipgloss") {
		t.Fatalf("uiOnly on a dashboard importing lipgloss = %q", got)
	}
}

// The rules table in docs/ARCHITECTURE.md is generated from rules; go test
// ./internal/archtest -update rewrites it.
func TestTheRulesAreDocumented(t *testing.T) {
	root, _ := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(docPath))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crlf := strings.Contains(string(raw), "\r\n")
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n")
	head, rest, ok := strings.Cut(doc, tableBegin+"\n")
	if !ok {
		t.Fatalf("%s lacks the %s marker", docPath, tableBegin)
	}
	body, tail, ok := strings.Cut(rest, tableEnd)
	if !ok {
		t.Fatalf("%s lacks the %s marker", docPath, tableEnd)
	}
	want := table(rules)
	if body == want {
		return
	}
	if !*update {
		t.Fatalf("the dependency rules table in %s is stale; run go test ./internal/archtest -update", docPath)
	}
	out := head + tableBegin + "\n" + want + tableEnd + tail
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, pkg string
		want         bool
	}{
		{"internal/agent", "internal/agent", true},
		{"internal/agent", "internal/agentx", false},
		{"internal/memory/**", "internal/memory", true},
		{"internal/memory/**", "internal/memory/semantic", true},
		{"internal/memory/**", "internal/memoryx", false},
		{"internal/plugins/*", "internal/plugins/web", true},
		{"internal/plugins/*", "internal/plugins", false},
		{"internal/plugins/*", "internal/plugins/web/x", false},
		{"**", "anything/at/all", true},
	} {
		if got := match(tc.pattern, tc.pkg); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.pkg, got, tc.want)
		}
	}
}

// The checker itself: a layering break, a package without a rule, and a
// plugin importing another plugin are all reported. Any internal package may
// import the event model, and nothing else may.
func TestViolationsReportsWhatTheRulesForbid(t *testing.T) {
	got := violations(map[string][]string{
		"internal/model":       {"internal/agent", "internal/logs/event"},
		"internal/newthing":    nil,
		"internal/plugins/web": {"internal/plugins/wiki", "internal/model", "internal/logs/event"},
		"internal/agent":       {"internal/memory/semantic", "internal/model/toolfmt"},
		"reference/logging":    {"internal/logs/event"},
	}, rules)
	want := []string{
		"internal/model imports internal/agent, which its rule (internal/model) does not allow",
		"internal/newthing: no dependency rule covers it; add one to rules in internal/archtest/rules.go",
		"internal/plugins/web imports internal/plugins/wiki, which its rule (internal/plugins/*) does not allow",
		"reference/logging imports internal/logs/event, which its rule (reference/**) does not allow",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("violations =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
