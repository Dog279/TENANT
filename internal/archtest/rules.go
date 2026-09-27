package archtest

import (
	"fmt"
	"sort"
	"strings"
)

// A rule says which module packages the packages matching Pkg may import.
// Patterns are module-relative: "x" is package x, "x/*" its direct children,
// "x/**" x and every package below it.
type rule struct {
	Pkg   string
	May   []string
	Notes string
}

// anything lets a package import any module package.
var anything = []string{"**"}

// rules is the module's dependency direction. The first rule that matches a
// package governs it, so specific rules come before the wider ones below
// them. A package no rule matches fails the test: a new package needs a
// decision here.
var rules = []rule{
	{Pkg: "cmd/**", May: anything, Notes: "the composition root and command adapters"},
	{Pkg: "scripts/**", Notes: "build tools stand alone"},
	{Pkg: "reference/**", May: []string{"reference/**"}, Notes: "examples for tool builders; never tenant/internal"},
	{Pkg: "internal/archtest", Notes: "these tests"},

	{Pkg: "internal/fsperm", Notes: "leaf"},
	{Pkg: "internal/secrets", Notes: "leaf"},
	{Pkg: "internal/datalock", Notes: "leaf"},
	{Pkg: "internal/httplimits", Notes: "leaf"},
	{Pkg: "internal/version", Notes: "leaf"},
	{Pkg: "internal/runtrace", Notes: "leaf"},
	{Pkg: "internal/cmdreg", Notes: "leaf"},
	{Pkg: "internal/cron", Notes: "leaf"},
	{Pkg: "internal/mcp/transport", Notes: "leaf"},
	{Pkg: "internal/model/profiles", Notes: "leaf"},
	{Pkg: "internal/control", Notes: "leaf: the data types the runtime, the TUI and the dashboard share"},
	{Pkg: "internal/pluginreg", Notes: "leaf: the plugin registry's invariants and indexes"},
	{Pkg: "internal/sandbox", Notes: "leaf: the exec-backend seam"},
	{Pkg: "internal/sandbox/**", May: []string{"internal/sandbox/**"}},

	{Pkg: "internal/logs", May: []string{"internal/fsperm"}, Notes: "any package may log through it"},
	{Pkg: "internal/research", May: []string{"internal/fsperm"}},
	{Pkg: "internal/backup", May: []string{"internal/datalock", "internal/fsperm", "internal/secrets", "internal/version"}},
	{Pkg: "internal/peering", May: []string{"internal/httplimits", "internal/secrets"}},
	{Pkg: "internal/mcp", May: []string{"internal/mcp/transport"}, Notes: "wire protocol only, no LLM knowledge"},
	{Pkg: "internal/model", May: []string{"internal/model/profiles", "internal/runtrace"}, Notes: "no memory, agent or plugins"},
	{Pkg: "internal/model/**", May: []string{"internal/model/**", "internal/runtrace"}},
	{Pkg: "internal/orchestra", May: []string{"internal/model"}},
	{Pkg: "internal/route", May: []string{"internal/model", "internal/fsperm"}},
	{Pkg: "internal/config", May: []string{"internal/fsperm", "internal/secrets", "internal/model", "internal/route", "internal/orchestra"}, Notes: "the persisted configuration; knows no runtime"},
	{Pkg: "internal/memory/**", May: []string{"internal/memory/**", "internal/model", "internal/fsperm"}, Notes: "tiers compose only through assemble, distill and compress"},
	{Pkg: "internal/plugins/*", May: []string{"internal/model", "internal/secrets", "internal/fsperm", "internal/memory/cosine"}, Notes: "capabilities never import each other"},
	{Pkg: "internal/eval/**", May: []string{"internal/model", "internal/route", "internal/memory/working"}},
	{Pkg: "internal/improve", May: []string{"internal/memory/**", "internal/model", "internal/fsperm"}},
	{Pkg: "internal/privacy", May: []string{"internal/memory/**", "internal/fsperm"}},
	{Pkg: "internal/mcpserver", May: []string{"internal/mcp/**", "internal/memory/**", "internal/model"}},
	{Pkg: "internal/agent", May: []string{"internal/logs", "internal/memory/**", "internal/model/**", "internal/orchestra", "internal/runtrace"}, Notes: "the turn loop; knows no plugin and no surface"},
	{Pkg: "internal/toolmux", May: []string{"internal/agent", "internal/model", "internal/memory/cosine", "internal/plugins/mcpremote", "internal/peering", "internal/control"}, Notes: "the tool registry and dispatcher; builds no plugin itself"},
	{Pkg: "internal/tui", May: []string{"internal/agent", "internal/cmdreg", "internal/control", "internal/fsperm", "internal/model"}, Notes: "never the dashboard"},
	{Pkg: "internal/dashboard", May: []string{"internal/agent", "internal/httplimits", "internal/logs", "internal/orchestra"}, Notes: "never the terminal UI"},
}

// match reports whether module-relative package path pkg matches pattern.
func match(pattern, pkg string) bool {
	switch {
	case pattern == "**":
		return true
	case strings.HasSuffix(pattern, "/**"):
		base := strings.TrimSuffix(pattern, "/**")
		return pkg == base || strings.HasPrefix(pkg, base+"/")
	case strings.HasSuffix(pattern, "/*"):
		base := strings.TrimSuffix(pattern, "/*")
		rest, ok := strings.CutPrefix(pkg, base+"/")
		return ok && rest != "" && !strings.Contains(rest, "/")
	}
	return pattern == pkg
}

// ruleFor returns the rule governing pkg, or false when none does.
func ruleFor(rs []rule, pkg string) (rule, bool) {
	for _, r := range rs {
		if match(r.Pkg, pkg) {
			return r, true
		}
	}
	return rule{}, false
}

// violations checks an import graph (module-relative package → the module
// packages it imports) against rs.
func violations(graph map[string][]string, rs []rule) []string {
	pkgs := make([]string, 0, len(graph))
	for p := range graph {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	var out []string
	for _, p := range pkgs {
		r, ok := ruleFor(rs, p)
		if !ok {
			out = append(out, fmt.Sprintf("%s: no dependency rule covers it; add one to rules in internal/archtest/rules.go", p))
			continue
		}
		for _, imp := range graph[p] {
			allowed := false
			for _, m := range r.May {
				if match(m, imp) {
					allowed = true
					break
				}
			}
			if !allowed {
				out = append(out, fmt.Sprintf("%s imports %s, which its rule (%s) does not allow", p, imp, r.Pkg))
			}
		}
	}
	return out
}

// table renders rs as the markdown table in docs/ARCHITECTURE.md §2.
func table(rs []rule) string {
	var b strings.Builder
	b.WriteString("| Package | May import | Note |\n|---|---|---|\n")
	for _, r := range rs {
		may := "nothing in the module"
		switch {
		case len(r.May) == 1 && r.May[0] == "**":
			may = "anything"
		case len(r.May) > 0:
			parts := make([]string, len(r.May))
			for i, m := range r.May {
				parts[i] = "`" + m + "`"
			}
			may = strings.Join(parts, ", ")
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.Pkg, may, r.Notes)
	}
	return b.String()
}

// uiOnly lists the packages outside the terminal UI and the composition root
// that import Bubble Tea (github.com/charmbracelet/...), from an external
// import graph.
func uiOnly(external map[string][]string) []string {
	pkgs := make([]string, 0, len(external))
	for p := range external {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	var out []string
	for _, p := range pkgs {
		if p == "internal/tui" || match("cmd/**", p) {
			continue
		}
		for _, imp := range external[p] {
			if strings.HasPrefix(imp, "github.com/charmbracelet/") {
				out = append(out, fmt.Sprintf("%s imports %s; only internal/tui and cmd/tenant may", p, imp))
			}
		}
	}
	return out
}
