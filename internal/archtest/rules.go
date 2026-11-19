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
	{Pkg: "internal/selfupdate", Notes: "leaf: finding, verifying and swapping in a release binary; tenant update sequences it"},
	{Pkg: "internal/runtrace", Notes: "leaf"},
	{Pkg: "internal/cmdreg", Notes: "leaf"},
	{Pkg: "internal/cron", Notes: "leaf"},
	{Pkg: "internal/mcp/transport", Notes: "leaf"},
	{Pkg: "internal/model/profiles", Notes: "leaf"},
	{Pkg: "internal/control", Notes: "leaf: the data types the runtime, the TUI and the dashboard share"},
	{Pkg: "internal/pluginreg", Notes: "leaf: the plugin registry's invariants and indexes, and the events every plugin has"},
	{Pkg: "internal/lifecycle", May: []string{"internal/logs/catalog"}, Notes: "the staged shutdown of a long-running command, filed in the system log, and the registry of what is running"},
	{Pkg: "internal/skillpkg", Notes: "leaf: the Agent Skills package format (SKILL.md)"},
	{Pkg: "internal/sandbox", Notes: "leaf: the exec-backend seam"},
	{Pkg: "internal/sandbox/**", May: []string{"internal/sandbox/**"}},
	{Pkg: "internal/logs/event", Notes: "leaf: the event model; every internal package may import it"},
	{Pkg: "internal/logs/event/eventtest", Notes: "tests' event recorder, on the event model alone"},

	{Pkg: "internal/logs/query", May: []string{"internal/logs/event"}, Notes: "the filter language; the store plans it into SQL"},
	{Pkg: "internal/logs/store", May: []string{"internal/logs/event", "internal/logs/query", "internal/fsperm", "internal/datalock"}, Notes: "logbook.db, the system of record"},
	{Pkg: "internal/logs", May: []string{"internal/logs/event", "internal/logs/catalog", "internal/logs/store", "internal/fsperm", "internal/runtrace", "internal/datalock", "internal/version"}, Notes: "any package may log through it; never secrets: the composition root hands the store its key"},
	{Pkg: "internal/logs/catalog", May: []string{"internal/logs/event"}, Notes: "the built-in event definitions"},
	{Pkg: "internal/logs/logapi", May: []string{"internal/logs", "internal/logs/event", "internal/logs/store", "internal/logs/query", "internal/logs/catalog", "internal/lifecycle", "internal/runtrace", "internal/version"}, Notes: "the log book's HTTP API: the dashboard serves it, tenant logs runs it in process"},
	{Pkg: "internal/logs/capture", May: []string{"internal/logs/event", "internal/runtrace", "internal/lifecycle"}, Notes: "child-process capture; every internal package may import it; a running child is a lifecycle component"},
	{Pkg: "internal/research", May: []string{"internal/fsperm"}},
	{Pkg: "internal/backup", May: []string{"internal/datalock", "internal/fsperm", "internal/secrets", "internal/version"}},
	{Pkg: "internal/fleet/shadow", May: []string{"internal/memory/episodic", "internal/memory/semantic", "internal/secrets", "internal/fsperm", "internal/datalock"}, Notes: "a fleet machine's memory: the master's shadow in RAM and its encrypted snapshots, the child's copies, and which side wins; knows no link"},
	{Pkg: "internal/peering", May: []string{"internal/httplimits", "internal/secrets"}},
	{Pkg: "internal/fleet", May: []string{"internal/fleet/shadow", "internal/secrets", "internal/datalock", "internal/httplimits", "internal/mcp/**", "internal/model"}, Notes: "a fleet's identity, enrollment and fleet.json, and the link between a master and its children; serves the tool mux it is handed and knows no tool and no command"},
	{Pkg: "internal/mcp", May: []string{"internal/mcp/transport"}, Notes: "wire protocol only, no LLM knowledge"},
	{Pkg: "internal/model", May: []string{"internal/model/profiles", "internal/runtrace"}, Notes: "no memory, agent or plugins"},
	{Pkg: "internal/model/**", May: []string{"internal/model/**", "internal/runtrace", "internal/fsperm"}},
	{Pkg: "internal/orchestra", May: []string{"internal/model"}},
	{Pkg: "internal/attach", May: []string{"internal/model", "internal/fsperm"}, Notes: "image attachments: sniff, scale, save; decodes every image a message carries"},
	{Pkg: "internal/route", May: []string{"internal/model", "internal/fsperm", "internal/datalock", "internal/runtrace"}},
	{Pkg: "internal/config", May: []string{"internal/fsperm", "internal/secrets", "internal/model", "internal/route", "internal/orchestra", "internal/logs/catalog"}, Notes: "the persisted configuration; knows no runtime; files each change in the Security log"},
	{Pkg: "internal/memory/**", May: []string{"internal/memory/**", "internal/model", "internal/fsperm"}, Notes: "tiers compose only through assemble, distill and compress"},
	{Pkg: "internal/plugins/*", May: []string{"internal/model", "internal/secrets", "internal/fsperm", "internal/datalock", "internal/httplimits", "internal/memory/cosine", "internal/pluginreg"}, Notes: "capabilities never import each other; each declares its events through pluginreg"},
	{Pkg: "internal/eval/**", May: []string{"internal/model", "internal/route", "internal/memory/working"}},
	{Pkg: "internal/improve", May: []string{"internal/memory/**", "internal/model", "internal/fsperm"}},
	{Pkg: "internal/privacy", May: []string{"internal/memory/**", "internal/fsperm", "internal/logs", "internal/logs/store"}},
	{Pkg: "internal/logs/alerts", May: []string{"internal/logs", "internal/logs/event", "internal/logs/query", "internal/logs/store"}, Notes: "alert rules, evaluated in the housekeeper-lease holder; the composition root sends the actions"},
	{Pkg: "internal/mcpserver", May: []string{"internal/mcp/**", "internal/memory/**", "internal/model"}},
	{Pkg: "internal/agent", May: []string{"internal/attach", "internal/logs", "internal/memory/**", "internal/model/**", "internal/orchestra", "internal/runtrace"}, Notes: "the turn loop; knows no plugin and no surface"},
	{Pkg: "internal/approval", May: []string{"internal/agent", "internal/control", "internal/logs", "internal/runtrace"}, Notes: "the approval decision point; knows no plugin and no surface"},
	{Pkg: "internal/toolmux", May: []string{"internal/agent", "internal/model", "internal/memory/cosine", "internal/plugins/mcpremote", "internal/peering", "internal/control", "internal/fsperm"}, Notes: "the tool registry and dispatcher; builds no plugin itself"},
	{Pkg: "internal/md/goldmark/**", May: []string{"internal/md/goldmark/**"}, Notes: "Tenant's copy of goldmark (MIT); imports itself and the standard library only"},
	{Pkg: "internal/md", May: []string{"internal/md/goldmark/**"}, Notes: "Markdown: parse, and HTML for the dashboard; the terminal app renders the tree itself"},
	{Pkg: "internal/uicomp", May: []string{"internal/model"}, Notes: "the components the model answers with: schema, caps, the note, HTML; the terminal app draws them itself"},
	{Pkg: "internal/tui", May: []string{"internal/agent", "internal/attach", "internal/cmdreg", "internal/control", "internal/fsperm", "internal/md/**", "internal/model", "internal/uicomp"}, Notes: "never the dashboard"},
	{Pkg: "internal/dashboard", May: []string{"internal/agent", "internal/control", "internal/httplimits", "internal/logs", "internal/logs/logapi", "internal/logs/query", "internal/logs/store", "internal/md", "internal/memory/working", "internal/orchestra", "internal/uicomp", "internal/version"}, Notes: "never the terminal UI"},
}

// everyone is one allowance on top of the rule that governs each internal
// package: the event model and the built-in providers (catalog), so an event
// or an audit record is written where the action happens, and child capture,
// so a child is filed where it is started, without widening the rules for the
// heavy logs packages. All three import only light packages: leaves, and
// lifecycle and the catalog, which import the event model alone.
// reference/** and scripts/** stay out of it: they must build on their own.
var everyone = rule{Pkg: "internal/**", May: []string{"internal/logs/event", "internal/logs/catalog", "internal/logs/capture"}, Notes: "on top of its own row: the event model, the built-in providers (the Security log's records among them) and child capture, so an event is written, and a child filed, where the action happens"}

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
			if !allows(r, imp) && !(match(everyone.Pkg, p) && allows(everyone, imp)) {
				out = append(out, fmt.Sprintf("%s imports %s, which its rule (%s) does not allow", p, imp, r.Pkg))
			}
		}
	}
	return out
}

// allows reports whether r lets its packages import imp.
func allows(r rule, imp string) bool {
	for _, m := range r.May {
		if match(m, imp) {
			return true
		}
	}
	return false
}

// table renders everyone, then rs, as the markdown table in
// docs/ARCHITECTURE.md §2.
func table(rs []rule) string {
	var b strings.Builder
	b.WriteString("| Package | May import | Note |\n|---|---|---|\n")
	for i, r := range append([]rule{everyone}, rs...) {
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
		pkg := "`" + r.Pkg + "`"
		if i == 0 {
			pkg = "every `" + r.Pkg + "` package"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", pkg, may, r.Notes)
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
