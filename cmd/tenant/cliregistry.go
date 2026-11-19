package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"tenant/internal/cmdreg"
	"tenant/internal/mcp"
	"tenant/internal/version"
)

// The CLI's command registry (DOC-003 / TEN-320). main dispatches through it,
// and `tenant help`, `tenant help all`, `tenant help <command>` and `tenant
// completion` render from it, so a command cannot be dispatched and left out
// of the help. The TUI's slash commands use the same registry type
// (internal/tui/cmd_registry.go).

// cliRun runs one subcommand with the arguments after its name.
type cliRun func(ctx context.Context, args []string) error

// cliCommand is one registered subcommand.
type cliCommand = cmdreg.Command[cliRun]

var cliSections = []cmdreg.Section{
	{ID: "start", Title: "Get started"},
	{ID: "run", Title: "Run Tenant"},
	{ID: "memory", Title: "Memory"},
	{ID: "models", Title: "Models and routing"},
	{ID: "agents", Title: "Agents, research and goals"},
	{ID: "plugins", Title: "One-shot plugin turns (read by default; --allow-* to act)"},
	{ID: "connect", Title: "Connections and federation"},
	{ID: "safety", Title: "Safety"},
	{ID: "data", Title: "Your data"},
	{ID: "dev", Title: "Diagnostics and development"},
}

// Flags most commands take (bindCommon).
var commonFlagNames = []string{"--backend", "--agent", "--data", "--config", "--log-dir", "--vllm-endpoint", "--vllm-model",
	"--vllm-tool-format", "--embed-endpoint", "--embed-model", "--embed-dim", "--api-key", "--plan-loop-ceiling"}

// withPluginCommands places every plugin's commands (plugin_<id>.go) before
// the first command of the connect section, where the plugin block sits.
func withPluginCommands(cmds []*cliCommand) []*cliCommand {
	at := len(cmds)
	for i, c := range cmds {
		if c.Section == "connect" {
			at = i
			break
		}
	}
	out := append([]*cliCommand{}, cmds[:at]...)
	out = append(out, pluginCommands()...)
	return append(out, cmds[at:]...)
}

// cliCommands declares every subcommand. It is a function, not a variable,
// because the help command reads the registry the list is built into.
func cliCommands() []*cliCommand {
	cmds := []*cliCommand{
		// Get started.
		{Names: []string{"setup"}, Section: "start", Run: cmdSetup,
			Summary: "one-time wizard: endpoints, model, gateway → config.json",
			Task:    "connect a model, local or cloud; run this first",
			Rows:    []cmdreg.Row{{Syntax: "setup [--provider KIND --vllm-endpoint URL]", Desc: "one-time wizard: endpoints, model, gateway → config.json"}}},
		{Names: []string{"tui"}, Section: "start", Run: cmdTUI,
			Summary: "full-screen terminal UI: streaming chat + live activity feed",
			Task:    "open the terminal app and start chatting (/help inside for commands)"},
		{Names: []string{"chat"}, Section: "start", Run: cmdChat,
			Summary: "interactive agent loop without the UI; one stdin line is one turn"},
		{Names: []string{"doctor"}, Section: "start", Run: cmdDoctor, Flags: []string{"--fix", "--json"},
			Summary: "diagnose the setup; --fix repairs what it safely can",
			Task:    "something wrong? find out what, and --fix it",
			Rows:    []cmdreg.Row{{Syntax: "doctor [--fix] [--json]", Desc: "diagnose the setup; --fix repairs what it safely can"}}},
		{Names: []string{"help", "-h", "--help"}, Section: "start", Run: cmdHelp, Sub: []string{"all"},
			Summary: "this help; `help all` for every command, `help <command>` for one",
			Rows: []cmdreg.Row{
				{Syntax: "help", Desc: "the commands you need first"},
				{Syntax: "help all", Desc: "every command, by section"},
				{Syntax: "help <command>", Desc: "one command's usage; `tenant <command> --help` lists its flags"},
			}},
		{Names: []string{"version", "-v", "--version"}, Section: "start", Run: cmdVersion,
			Summary: "print the version and build provenance"},
		{Names: []string{"update"}, Section: "start", Run: cmdUpdate, Flags: []string{"--check", "--version", "--yes", "--now", "--force", "--rollback"},
			Summary: "install the latest release: verify, back up, swap the binary, restart, confirm; --check only looks",
			Rows: []cmdreg.Row{
				{Syntax: "update [--version vX.Y.Z] [--yes] [--now] [--force]", Desc: "download and verify the release, back up the data, swap the binary, restart the service and confirm it; the old binary comes back if the hub does not"},
				{Syntax: "update --check", Desc: "one line, running vs latest; exit 10 when an update is available"},
				{Syntax: "update --rollback", Desc: "put the previous binary back and print the data-restore command"},
			}},
		{Names: []string{"completion"}, Section: "start", Run: cmdCompletion, Sub: []string{"bash", "zsh", "fish", "powershell"},
			Summary: "print a shell completion script",
			Rows: []cmdreg.Row{
				{Syntax: "completion bash", Desc: "source <(tenant completion bash)   in ~/.bashrc"},
				{Syntax: "completion zsh", Desc: "source <(tenant completion zsh)    in ~/.zshrc, after compinit"},
				{Syntax: "completion fish", Desc: "tenant completion fish | source   or save it to ~/.config/fish/completions/tenant.fish"},
				{Syntax: "completion powershell", Desc: "tenant completion powershell | Out-String | Invoke-Expression   in $PROFILE"},
			}},

		// Run Tenant.
		{Names: []string{"serve"}, Section: "run", Run: cmdServe,
			Flags:   []string{"--self-improve", "--distill-every", "--profile-every", "--eval-every", "--dashboard", "--dashboard-addr", "--allow-no-memory"},
			Summary: "run the headless 24/7 hub: the TUI's brain with the web dashboard as its control, chat and approval surface",
			Task:    "keep Tenant running in the background; drive it from the dashboard",
			Rows: []cmdreg.Row{
				{Syntax: "serve", Desc: "the headless 24/7 hub: the same memory, tools and approvals as the TUI, driven from the web dashboard (chat, approvals, settings)"},
				{Syntax: "", Desc: "runs self-improvement (distill, consolidate, profile, skills; nightly eval with --eval-every), cron jobs, retention, MCP connectors, the peer listener when federation is on, and the Discord relay when enabled"},
				{Syntax: "serve --dashboard-addr ADDR", Desc: "listen elsewhere than 127.0.0.1:9770; --dashboard=false runs without the panel"},
			}},
		{Names: []string{"attach"}, Section: "run", Run: cmdAttach, Flags: []string{"--follow", "--token", "--since", "--interval", "--source", "--level", "--trace"},
			Summary: "watch a running hub's activity feed",
			Rows:    []cmdreg.Row{{Syntax: "attach [URL] [--follow] [--source discord]", Desc: "watch a hub's activity feed; with no URL and logbook.db readable here, as logs query --follow"}}},
		{Names: []string{"service"}, Section: "run", Run: cmdService, Sub: []string{"install", "uninstall", "status", "start", "stop", "restart"}, Flags: []string{"--print", "--system", "--now"},
			Summary: "run serve in the background under the OS: install, status, start, stop, restart, uninstall",
			Rows: []cmdreg.Row{
				{Syntax: "service install [--print] [--system]", Desc: "write the unit (a LaunchAgent, a systemd user unit, a logon task; --system: a Windows service), enable it, start it; --print only shows it"},
				{Syntax: "service status", Desc: "installed? running (version, turn_active)? where the logs are"},
				{Syntax: "service start | stop | restart [--now]", Desc: "through the service manager; restart waits for the running turn to finish unless --now"},
				{Syntax: "service uninstall", Desc: "stop it, disable it, remove the unit"},
			}},
		{Names: []string{"orchestrate", "team"}, Section: "run", Run: cmdOrchestrate,
			Summary: "multi-agent team: an orchestrator spawns concurrent sub-agents that talk over a live bus",
			Rows:    []cmdreg.Row{{Syntax: `orchestrate "<task>"`, Desc: "multi-agent team: an orchestrator spawns concurrent sub-agents that talk over a live bus and self-resolve"}}},
		{Names: []string{"logs"}, Section: "run", Run: cmdLogs,
			Sub: []string{"query", "show", "trace", "stats", "export", "verify", "config", "clear", "debug", "catalog", "components", "recover", "alerts"},
			Flags: []string{"--format", "--url", "--token", "--follow", "--limit", "--reverse", "--bucket", "--out", "--include-sensitive", "--audit", "--since", "--until",
				"--bundle", "--max-size", "--max-age", "--on-full", "--min-level", "--enabled", "--reset", "--export", "--for", "--id",
				"--count", "--window", "--action", "--target", "--throttle"},
			Summary: "read and manage the log book: query, follow, trace, export, verify, configure",
			Rows: []cmdreg.Row{
				{Syntax: "logs", Desc: "the logs: events, size, limit, how long they are kept, last write"},
				{Syntax: "logs query [FILTER] [--follow] [--limit N] [--reverse]", Desc: "the newest N events that match, oldest first; --follow keeps printing new ones"},
				{Syntax: "logs show <seq>", Desc: "one event in full, with its catalog help and how often it has happened"},
				{Syntax: "logs trace <trace|activity>", Desc: "a turn's or an operation's timeline across every log, with its run record"},
				{Syntax: "logs stats [FILTER] [--bucket 1h]", Desc: "counts by log and level, over time, and the most frequent event IDs"},
				{Syntax: "logs export [FILTER] --out FILE [--include-sensitive]", Desc: "every match, after a header record (NDJSON; --format csv|text); --audit: the Security log's audit bundle"},
				{Syntax: "logs verify [security] [--bundle FILE]", Desc: "the integrity check; for security, its chain against the key and the anchor"},
				{Syntax: "logs config <log> [--max-size 64MB] [--max-age 30d] [--on-full keep] [--min-level warning]", Desc: "show or change a log's policy (--reset: config.json's)"},
				{Syntax: "logs clear <log> [--export FILE]", Desc: "delete a log's events (never security's)"},
				{Syntax: "logs debug <provider|log> --for 30m", Desc: "write a log's verbose events for a while (--for 0 stops)"},
				{Syntax: "logs catalog [provider] [--id N]", Desc: "the event reference"},
				{Syntax: "logs components", Desc: "what is running now"},
				{Syntax: "logs recover [FILE]", Desc: "copy what can still be read of a quarantined logbook.db.corrupt-<time> into the log book"},
				{Syntax: "logs alerts [list|enable|disable|remove <rule>]", Desc: "the alert rules: the built-in ones and logs.alerts; remove puts a built-in one back to its defaults"},
				{Syntax: "logs alerts add <rule> FILTER [--count N --window 10m] [--action discord|imessage|cron] [--target PROMPT] [--throttle 1h]", Desc: "add a rule, or replace the one of that name"},
				{Syntax: "", Desc: "reads take --format text|json|csv; logbook.db is read here when this account can open it, else through the dashboard (--url forces one)"},
			}},
		{Names: []string{"log"}, Section: "run", Run: cmdLog, Flags: []string{"--level", "--list", "--url", "--token", "--file"},
			Summary: "file a line into the log book",
			Rows:    []cmdreg.Row{{Syntax: `log <source> "<msg>" [k=v…]`, Desc: "file a line under <source>; the folder and dashboard tab appear on first use (--level warn|error; --list)"}}},

		// Memory.
		{Names: []string{"memory"}, Section: "memory", Run: cmdMemory, Sub: []string{"search", "import", "reembed"},
			Summary: "search memory, import notes, re-embed",
			Task:    "find something Tenant remembers",
			Rows: []cmdreg.Row{
				{Syntax: "memory search <query>", Desc: "search episodes + facts"},
				{Syntax: "memory import <file.md>", Desc: "import operator notes as facts (--protected, --importance N, --dry-run)"},
				{Syntax: "memory reembed", Desc: "recompute every embedding with the current embedder"},
			}},
		{Names: []string{"distill"}, Section: "memory", Run: cmdDistill,
			Summary: "run one episodic → semantic distillation pass"},
		{Names: []string{"consolidate"}, Section: "memory", Run: cmdConsolidate, Flags: []string{"--dry-run", "--threshold", "--holistic"},
			Summary: "merge near-duplicate facts (--dry-run, --threshold, --holistic)"},
		{Names: []string{"ack"}, Section: "memory", Run: func(ctx context.Context, args []string) error { return cmdFeedback(ctx, args, "ack") },
			Summary: "mark the last turn good (the self-improvement success signal)"},
		{Names: []string{"undo"}, Section: "memory", Run: func(ctx context.Context, args []string) error { return cmdFeedback(ctx, args, "undo") },
			Summary: "mark the last turn bad (suspends trusted skill auto-accept)"},
		{Names: []string{"skills"}, Section: "memory", Run: cmdSkills,
			Sub:     []string{"list", "show", "import", "export", "trust", "untrust", "seed", "auto"},
			Flags:   []string{"--trust", "--agent"},
			Summary: "the skill library: list, review and trust skills, import and export SKILL.md packages",
			Rows: []cmdreg.Row{
				{Syntax: "skills list", Desc: "every skill, with its state, trust and origin"},
				{Syntax: "skills show <name>", Desc: "one skill in full: trust, origin, package, required tools, instructions"},
				{Syntax: "skills trust|untrust <name>", Desc: "only a trusted skill is offered to the agent (what it saves, and imports, start untrusted)"},
				{Syntax: "skills import <dir|SKILL.md> [--trust]", Desc: "import an Agent Skills package (untrusted unless --trust)"},
				{Syntax: "skills export <name> <dir>", Desc: "write the skill as <dir>/<name>/SKILL.md"},
				{Syntax: "skills seed <bundle>", Desc: "install a starter skill bundle (e.g. gstack)"},
				{Syntax: "skills auto [off|on|trusted]", Desc: "auto-accept induced skills (trusted = only while feedback is healthy)"},
			}},

		// Models and routing.
		{Names: []string{"model", "models"}, Section: "models", Run: cmdModel, Sub: []string{"list", "show", "use", "models", "add", "remove"},
			Summary: "manage model backends and set the primary (also /model in the TUI)",
			Rows: []cmdreg.Row{
				{Syntax: "model list | show <name>", Desc: "configured backends and the active one"},
				{Syntax: "model use <name> [model]", Desc: "switch the primary, optionally pinning a model variant"},
				{Syntax: "model models [name]", Desc: "list the models a provider serves (live)"},
				{Syntax: "model add <name> --endpoint URL [--kind K]", Desc: "register a backend"},
				{Syntax: "model remove <name>", Desc: "delete a backend (not the active one)"},
			}},
		{Names: []string{"route"}, Section: "models", Run: cmdRoute, Sub: []string{"status", "on", "off", "shadow", "assisted", "everywhere", "stats", "export-dataset"},
			Summary: "inspect or configure prompt routing (also /route in the TUI)",
			Rows: []cmdreg.Row{
				{Syntax: "route status", Desc: "router state and a classifier health probe"},
				{Syntax: "route on|off|shadow|assisted|everywhere", Desc: "persist one rollout stage"},
				{Syntax: "route stats", Desc: "big/disagreement/failure rates, latency, breaker trips"},
				{Syntax: "route export-dataset [--out FILE]", Desc: "export routing decisions as a training set"},
			}},
		{Names: []string{"eval"}, Section: "models", Run: cmdEval, Flags: []string{"--subset", "--json", "--quiet", "--list", "--compaction", "--gate-only", "--quality-model", "--baseline-check"},
			Summary: "run the eval harness",
			Rows:    []cmdreg.Row{{Syntax: "eval --subset smoke [--json]", Desc: "run the eval harness (also --compaction, --gate-only, --baseline-check FILE)"}}},

		// Agents, research and goals.
		{Names: []string{"agents", "agent"}, Section: "agents", Run: cmdAgents, Sub: []string{"list", "add", "model", "rename", "soul", "show", "remove"},
			Summary: "named sub-agent profiles (also /agents in the TUI)",
			Rows: []cmdreg.Row{
				{Syntax: "agents list | show <name>", Desc: "named sub-agent profiles"},
				{Syntax: "agents add <name> <provider> [model]", Desc: "register a sub-agent"},
				{Syntax: "agents model|rename|soul|remove …", Desc: "change or delete one"},
			}},
		{Names: []string{"research"}, Section: "agents", Run: cmdResearch, Sub: []string{"list", "show", "delete"},
			Summary: "deep research: plan → parallel web researchers → cited report",
			Rows: []cmdreg.Row{
				{Syntax: `research "<question>" [--out FILE]`, Desc: "deep research: plan → parallel web researchers → cited report"},
				{Syntax: "research list | show <id> | delete <id>", Desc: "past runs"},
			}},
		{Names: []string{"goal"}, Section: "agents", Run: cmdGoal, Flags: []string{"--max-turns", "--verbose"},
			Summary: "keep working toward a condition until a judge says it holds",
			Rows:    []cmdreg.Row{{Syntax: `goal "<condition>" [--max-turns N]`, Desc: "keep working toward a condition until a judge says it holds"}}},
		{Names: []string{"review"}, Section: "agents", Run: cmdReview, Flags: []string{"--reviewers"},
			Summary: "cascading CEO / Eng / Designer review appended to a plan file",
			Rows:    []cmdreg.Row{{Syntax: "review <plan.md> [--reviewers ceo,eng,design]", Desc: "cascading CEO / Eng / Designer review appended to the plan"}}},

		// Connections and federation.
		{Names: []string{"mcp"}, Section: "connect", Run: cmdMCP, Sub: []string{"connect"}, Flags: []string{"--trust-annotations", "--callback", "--no-browser"},
			Summary: "connect to a remote MCP server and list its tools",
			Rows:    []cmdreg.Row{{Syntax: "mcp connect <url>", Desc: "connect to a remote MCP server (OAuth 2.1 + DCR browser sign-in) and list its tools"}}},
		{Names: []string{"peer"}, Section: "connect", Run: cmdPeer, Sub: []string{"invite", "join", "list", "show", "remove", "revoke", "rotate", "share", "rename"},
			Summary: "federate Tenant instances: mutual-consent pairing and share policy",
			Rows: []cmdreg.Row{
				{Syntax: "peer invite | join …", Desc: "pair two Tenants with mutual consent"},
				{Syntax: "peer list | show | rename | remove | revoke | rotate", Desc: "manage paired peers"},
				{Syntax: "peer share …", Desc: "what each peer may see"},
			}},
		{Names: []string{"mcp-memory"}, Section: "connect", Run: cmdMCPMemory,
			Summary: "serve Tenant's memory over MCP (stdio) to another client"},

		// Safety.
		{Names: []string{"security"}, Section: "safety", Run: cmdSecurity, Sub: []string{"audit"}, Flags: []string{"--json"},
			Summary: "inspect the effective security posture",
			Rows:    []cmdreg.Row{{Syntax: "security audit [--json]", Desc: "what each live surface may do without approval, the browser sandbox, listeners, secret-file protection; non-zero on a critical finding"}}},
		{Names: []string{"permissions"}, Section: "safety", Run: cmdPermissions, Sub: []string{"effective", "preset"},
			Summary: "show or preset composed authority",
			Rows: []cmdreg.Row{
				{Syntax: "permissions effective", Desc: "composed authority by agent and surface"},
				{Syntax: "permissions preset safe|balanced|full", Desc: "apply a category preset"},
			}},
		{Names: []string{"sandbox"}, Section: "safety", Run: cmdSandbox, Sub: []string{"probe", "selftest", "run", "stop-all", "redteam"},
			Summary: "the execution sandbox: probe, escape self-test, run, red-team",
			Rows: []cmdreg.Row{
				{Syntax: "sandbox probe", Desc: "what each exec profile enforces on this host"},
				{Syntax: "sandbox selftest | redteam", Desc: "the escape suite, and the model-in-the-loop containment eval"},
				{Syntax: "sandbox run --surface S -- CMD", Desc: "run one command under a surface's profile"},
			}},

		// Your data.
		{Names: []string{"backup"}, Section: "data", Run: cmdBackup, Sub: []string{"create", "verify", "restore"},
			Flags:   []string{"--out", "--encrypt", "--passphrase-file", "--recipient", "--recipients-file", "--identity", "--json", "--dry-run", "--yes", "--force"},
			Summary: "one-file backup of the config and data dirs; verify it; restore it",
			Task:    "take a backup you can restore anywhere (--encrypt keeps your keys in it)",
			Rows: []cmdreg.Row{
				{Syntax: "backup [create] [--out FILE|DIR] [--encrypt]", Desc: "one file with every store, the archive and your config (--encrypt or --recipient also carries credentials)"},
				{Syntax: "backup verify FILE", Desc: "check every checksum, database, log and schema in it"},
				{Syntax: "backup restore FILE [--dry-run]", Desc: "put it back after checking it; the replaced dirs are kept"},
			}},
		{Names: []string{"privacy"}, Section: "data", Run: cmdPrivacy, Sub: []string{"status", "retention", "prune", "forget", "sessions", "export"},
			Flags:   []string{"--session", "--episode", "--fact", "--skill", "--match", "--before", "--archive-month", "--connector", "--only-agent", "--yes", "--out", "--no-archive", "--limit"},
			Summary: "what Tenant keeps and for how long; forget something; export everything",
			Task:    "see what Tenant keeps, and forget something",
			Rows: []cmdreg.Row{
				{Syntax: "privacy [status]", Desc: "every data class, how long it is kept, and how to remove it"},
				{Syntax: "privacy retention set <class> <days|forever>", Desc: "set a retention period"},
				{Syntax: "privacy forget --match \"text\" | --session ID | …", Desc: "remove specific data, with a preview first"},
				{Syntax: "privacy export --out DIR", Desc: "everything Tenant knows, as readable files"},
			}},
		{Names: []string{"runs"}, Section: "data", Run: cmdRuns, Sub: []string{"export"}, Flags: []string{"--out"},
			Summary: "export durable run records as JSONL",
			Rows:    []cmdreg.Row{{Syntax: "runs export [--out FILE]", Desc: "export durable run records as JSONL"}}},

		// Diagnostics and development.
		{Names: []string{"mcp-selftest"}, Section: "dev", Run: cmdMCPSelftest,
			Summary: "spawn mcp-memory as a subprocess and exercise the protocol against it"},
		{Names: []string{"tool-test"}, Section: "dev", Run: cmdToolTest,
			Summary: "run one tool-calling agent turn (the hardening harness)"},
	}
	return withPluginCommands(cmds)
}

var (
	cliOnce sync.Once
	cliReg  *cmdreg.Registry[cliRun]
)

func cliRegistry() *cmdreg.Registry[cliRun] {
	cliOnce.Do(func() { cliReg = cmdreg.MustNew(cliSections, cliCommands()) })
	return cliReg
}

// debugAttachNote is what `tenant version` and doctor say about a build made
// with -tags debugattach (TEN-535).
const debugAttachNote = "built with -tags debugattach: process protection is compiled out, so a debugger, or any other program of this user, can read this process"

func cmdVersion(context.Context, []string) error {
	fmt.Printf("tenant %s — speaks MCP %s (Go)\n", version.String(), mcp.ProtocolVersion)
	if protection.Disabled {
		fmt.Println(debugAttachNote)
	}
	return nil
}

// cmdHelp is `tenant help [all|<command>]`.
func cmdHelp(_ context.Context, args []string) error {
	switch {
	case len(args) == 0:
		writeBeginnerHelp(os.Stdout)
	case strings.EqualFold(args[0], "all"):
		writeFullHelp(os.Stdout)
	default:
		c := cliRegistry().Lookup(args[0])
		if c == nil {
			return fmt.Errorf("no command named %q; `tenant help all` lists them", args[0])
		}
		writeCommandHelp(os.Stdout, c)
	}
	return nil
}

const helpWidth = 44

// writeBeginnerHelp is `tenant` and `tenant help`: the jobs a new operator
// does first, not the whole reference.
func writeBeginnerHelp(w io.Writer) {
	fmt.Fprint(w, "tenant — your own AI agent: one binary, local or cloud models, a memory that learns.\n\n")
	fmt.Fprint(w, "What do you want to do?\n")
	var rows []cmdreg.Row
	for _, c := range cliRegistry().Tasks() {
		rows = append(rows, cmdreg.Row{Syntax: "tenant " + c.Names[0], Desc: c.Task})
	}
	fmt.Fprint(w, cmdreg.Table(rows, "  ", 24))
	fmt.Fprint(w, "\nEvery command: tenant help all   ·   one command: tenant help <command>\n")
	fmt.Fprint(w, "Most commands take --data DIR and --config DIR (default: the OS data and config dirs).\n")
}

// writeFullHelp is `tenant help all`: every command, by section.
func writeFullHelp(w io.Writer) {
	fmt.Fprint(w, "tenant — every command\n\nUSAGE\n  tenant <command> [flags]   (tenant <command> --help lists a command's flags)\n")
	for _, sec := range cliRegistry().Sections() {
		rows := prefixRows(cliRegistry().Rows(sec.ID))
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", strings.ToUpper(sec.Title[:1])+sec.Title[1:])
		fmt.Fprint(w, cmdreg.Table(rows, "  ", helpWidth))
	}
	fmt.Fprint(w, "\nCOMMON FLAGS\n")
	fmt.Fprint(w, cmdreg.Table([]cmdreg.Row{
		{Syntax: "--backend echo|vllm|anthropic|claudecode", Desc: "inference backend (default: config.json's provider, else echo: deterministic, offline)"},
		{Syntax: "--agent ID", Desc: `agent id (default "main")`},
		{Syntax: "--data DIR", Desc: "data dir (default OS data dir)"},
		{Syntax: "--config DIR", Desc: "config dir (default OS config dir)"},
	}, "  ", 24))
}

// writeCommandHelp is `tenant help <command>`.
func writeCommandHelp(w io.Writer, c *cliCommand) {
	fmt.Fprintf(w, "tenant %s — %s\n\n", c.Names[0], c.Summary)
	fmt.Fprint(w, cmdreg.Table(prefixRows(c.HelpRows()), "  ", helpWidth))
	if len(c.Names) > 1 {
		fmt.Fprintf(w, "\nAlso: %s\n", strings.Join(c.Names[1:], ", "))
	}
	fmt.Fprintf(w, "\nFlags: tenant %s --help\n", c.Names[0])
}

// prefixRows adds "tenant " to each row's syntax; a row without one continues
// the row above it.
func prefixRows(rows []cmdreg.Row) []cmdreg.Row {
	out := make([]cmdreg.Row, len(rows))
	for i, r := range rows {
		out[i] = r
		if r.Syntax != "" {
			out[i].Syntax = "tenant " + r.Syntax
		}
	}
	return out
}
