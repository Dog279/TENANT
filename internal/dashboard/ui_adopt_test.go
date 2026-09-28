package dashboard

// ui_adopt_test.go holds the allowlists of the page sweeps in ui_test.go
// (TEN-211): today's offenders, recorded by the foundation. WP-ux-adopt owns
// this file and removes each entry as it fixes the page, until both are
// empty. A sweep fails on an entry that no longer matches anything, so the
// lists only shrink.

// inlineJSAllow lists templates that still carry a <script> or an on*=
// handler (the native confirm() prompts and the Quality page's reload).
var inlineJSAllow = map[string]bool{"access.html": true, "eval.html": true}

// plainLanguageAllow lists, per template file (the template sweep) and per
// route (the rendered-page sweep), the banned words still on it.
var plainLanguageAllow = map[string][]string{
	// templates
	"access.html":            {"allowlist", "handles", "exec mode", "raw category id"},
	"activity.html":          {"emoji status marker"},
	"cron.html":              {"gated", "cron-exec", "turn", "raw category id"},
	"dashboard.html":         {"emoji status marker"},
	"eval.html":              {"planner"},
	"integrations.html":      {"gated", "slash command"},
	"keys.html":              {"credentials.json"},
	"mcp.html":               {"gated", "raw category id", "emoji status marker"},
	"memnav.html":            {"soul", "compaction"},
	"memory.html":            {"soul", "superseded"},
	"memory_facts.html":      {"turn"},
	"memory_provenance.html": {"compaction", "turn"},
	"memory_soul.html":       {"soul"},
	"memory_temporal.html":   {"superseded"},
	"toolrow.html":           {"emoji status marker"},
	"tools.html":             {"posture", "raw category id"},

	// rendered pages (renderAllPages(failing))
	"/access":                {"allowlist", "handles", "exec mode", "egress", "raw category id"},
	"/activity":              {"emoji status marker"},
	"/cron":                  {"gated", "cron-exec", "turn", "raw category id"},
	"/eval":                  {"planner"},
	"/integrations":          {"gated", "slash command"},
	"/mcp":                   {"gated", "raw category id", "emoji status marker"},
	"/memory":                {"soul", "compaction", "superseded"},
	"/memory/facts":          {"soul", "compaction"},
	"/memory/facts/1":        {"soul", "compaction"},
	"/memory/facts/removed":  {"soul", "compaction"},
	"/memory/facts/temporal": {"soul", "compaction", "superseded"},
	"/memory/provenance":     {"soul", "compaction", "turn"},
	"/memory/soul":           {"soul", "compaction"},
	"/settings/keys":         {"credentials.json"},
	"/tools":                 {"posture", "raw category id", "emoji status marker"},
}

// titleAllow lists the nav pages whose title isn't their nav label yet
// (DESIGN.md §2: a page's title is its nav label), by route, with the title
// each shows today.
var titleAllow = map[string]string{"/activity": "Activity", "/cron": "Cron", "/settings/keys": "Keys"}
