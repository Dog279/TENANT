package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"tenant/internal/sandbox"
	"tenant/internal/secrets"
)

type auditSeverity string

const (
	auditWarning  auditSeverity = "warning"
	auditCritical auditSeverity = "critical"
)

type securityFinding struct {
	ID       string        `json:"id"`
	Area     string        `json:"area"`
	Severity auditSeverity `json:"severity"`
	Summary  string        `json:"summary"`
	Remedy   string        `json:"remedy,omitempty"`
}

type listenerAudit struct {
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Address  string `json:"address,omitempty"`
	Loopback bool   `json:"loopback"`
	TLS      bool   `json:"tls"`
	Auth     bool   `json:"auth"`
	Note     string `json:"note,omitempty"`
}

type browserAudit struct {
	Enabled       bool `json:"enabled"`
	NoSandbox     bool `json:"no_sandbox"`
	SandboxActive bool `json:"sandbox_active"`
}

type webAudit struct {
	Enabled             bool   `json:"enabled"`
	EgressPolicy        string `json:"egress_policy"`
	RemoteReader        string `json:"remote_reader"`
	RemoteReaderEnabled bool   `json:"remote_reader_enabled"`
}

type remoteMCPAudit struct {
	Servers          []string `json:"servers"`
	TrustAnnotations bool     `json:"trust_annotations"`
	AllowWrite       bool     `json:"allow_write_without_approval"`
}

type fileProtectionAudit struct {
	Path      string `json:"path"`
	Sensitive bool   `json:"sensitive"`
	Status    string `json:"status"`
	Mode      string `json:"mode,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type secretStorageAudit struct {
	ProviderSecrets string `json:"provider_secrets"`
	MCPTokenCache   string `json:"mcp_token_cache"`
	DashboardAuth   string `json:"dashboard_auth"`
}

// executionAudit is where commands run. Backend, Isolation and FilesystemRoot
// are the local surface's (the TUI, dashboard and serve chat); Surfaces has
// every exec surface, resolved from exec_profiles and probed on this host.
type executionAudit struct {
	Backend        string         `json:"backend"`
	Isolation      string         `json:"isolation"` // none when its commands run unconfined; unavailable when they don't run
	Enabled        bool           `json:"enabled"`
	ModelBackend   string         `json:"model_backend"`
	FilesystemRoot string         `json:"filesystem_root"`
	Surfaces       []surfaceProbe `json:"surfaces"`
}

// auditExecution fills executionAudit from the live exec profiles.
func auditExecution(c *commonFlags, enabled bool, modelBackend string) executionAudit {
	e := executionAudit{Enabled: enabled, ModelBackend: modelBackend}
	for _, s := range execSurfaces {
		e.Surfaces = append(e.Surfaces, probeSurface(context.Background(), s, c.cfgDir, c.dataDir))
	}
	local := e.Surfaces[0] // execSurfaces starts with local
	e.Backend = local.Backend
	switch {
	case local.unconfined():
		e.Isolation, e.FilesystemRoot = sandbox.IsolationNone.String(), "unrestricted host filesystem"
	case local.failsClosed():
		e.Isolation, e.FilesystemRoot = "unavailable", "none: its commands do not run"
	default:
		e.Isolation, e.FilesystemRoot = local.Isolation, "confined by the "+local.Backend+" backend (filesystem "+local.Caps.Filesystem+")"
	}
	return e
}

// surfaceAudit is one authority surface: whether it is live, and what its
// broker would decide per category (TEN-338). Inactive surfaces are listed so
// the report shows what enabling one would grant; only active ones produce
// findings.
type surfaceAudit struct {
	authoritySurface
	Preset     string                       `json:"preset"`
	Categories map[string]effectiveCategory `json:"categories"`
}

type securityAuditReport struct {
	Agent     string          `json:"agent"`
	Listeners []listenerAudit `json:"listeners"`
	Browser   browserAudit    `json:"browser"`
	Web       webAudit        `json:"web"`
	// Permissions is the local surface, as before surfaces were added.
	Permissions   effectivePermissions  `json:"permissions"`
	Surfaces      []surfaceAudit        `json:"surfaces"`
	RemoteMCP     remoteMCPAudit        `json:"remote_mcp"`
	Files         []fileProtectionAudit `json:"file_protection"`
	SecretStorage secretStorageAudit    `json:"secret_storage"`
	Execution     executionAudit        `json:"execution"`
	Logbook       logbookAudit          `json:"logbook"`
	Findings      []securityFinding     `json:"findings"`
	Critical      int                   `json:"critical"`
	Warnings      int                   `json:"warnings"`
	Safe          bool                  `json:"safe"`
}

func cmdSecurity(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "audit" {
		return fmt.Errorf("usage: tenant security audit [--json] [common/plugin flags]")
	}
	return cmdSecurityAudit(ctx, args[1:])
}

func cmdSecurityAudit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("security audit", flag.ContinueOnError)
	c := bindCommon(fs)
	pf := bindPluginFlags(fs)
	asJSON := fs.Bool("json", false, "machine-readable output (for CI)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The audit reports file protection, so resolving the config must not
	// tighten those files first: reading credentials.json used to harden it
	// before the checks below ran (TEN-338).
	c.auditOnly = true
	if err := c.resolve(); err != nil {
		return err
	}
	applyPluginConfig(c, pf)
	_, _, _ = loadExecProfilesFromSettings(c.cfgDir, c.agent) // an unreadable file fails the compose below
	permissions, err := composeEffectivePermissions(c, pf, permissionSurfaceLocal)
	if err != nil {
		return fmt.Errorf("compose permissions: %w", err)
	}
	surfaces, err := composeSurfaceAudits(c, pf, runtime.GOOS)
	if err != nil {
		return fmt.Errorf("compose permissions: %w", err)
	}
	report := buildSecurityAuditReport(c, pf, permissions)
	report.Surfaces = surfaces
	var logbookFiles []fileProtectionAudit
	report.Logbook, logbookFiles = auditLogbook(ctx, c)
	report.Files = append(report.Files, logbookFiles...)
	evaluateSecurityAudit(&report)

	if *asJSON {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	} else {
		printSecurityAudit(report)
	}
	return securityAuditError(report)
}

func securityAuditError(report securityAuditReport) error {
	if report.Critical == 0 {
		return nil
	}
	return fmt.Errorf("security audit found %d critical unsafe configuration(s)", report.Critical)
}

// composeSurfaceAudits composes every authority surface, active or not.
func composeSurfaceAudits(c *commonFlags, pf *pluginFlags, goos string) ([]surfaceAudit, error) {
	var out []surfaceAudit
	for _, surface := range authoritySurfaces(c.lc, goos) {
		effective, err := composeEffectivePermissions(c, pf, surface.Surface)
		if err != nil {
			return nil, err
		}
		out = append(out, surfaceAudit{authoritySurface: surface, Preset: effective.Preset, Categories: effective.Categories})
	}
	return out, nil
}

func buildSecurityAuditReport(c *commonFlags, pf *pluginFlags, permissions effectivePermissions) securityAuditReport {
	webEnabled := webFlagsOf(pf).on || toolOverrideEnabled(permissions.ToolOverrides, "web_")
	// Remote reader is opt-in (default off) since TEN-319; readerEnabled reads
	// the persisted/env opt-in state.
	remoteReaderOn := webEnabled && readerEnabled(c.lc)
	browserNoSandbox := c.lc != nil && c.lc.BrowserNoSandbox
	listeners := configuredListeners(c.lc)
	servers := append([]string{}, c.lc.MCPRemotes...)
	servers = append(servers, pf.mcpRemotes...)
	sort.Strings(servers)

	modelBackend := c.genKind
	if modelBackend == "" {
		modelBackend = c.backend
	}
	report := securityAuditReport{
		Agent:     c.agent,
		Listeners: listeners,
		Browser: browserAudit{
			Enabled: webEnabled, NoSandbox: browserNoSandbox,
			SandboxActive: webEnabled && !browserNoSandbox,
		},
		Web: webAudit{
			Enabled: webEnabled, EgressPolicy: "private/loopback/link-local/metadata destinations blocked by default (TEN-307 egress guard); named private endpoints only via --web-private-network",
			RemoteReader: "Jina Reader (r.jina.ai)", RemoteReaderEnabled: remoteReaderOn,
		},
		Permissions: permissions,
		RemoteMCP: remoteMCPAudit{
			Servers: servers, TrustAnnotations: pf.mcpTrustAnnotations, AllowWrite: pf.mcpAllowWrite,
		},
		Files: protectedFileSpotChecks(c.cfgDir, c.dataDir, c.agent, c.lc.Dashboard.Auth != ""),
		SecretStorage: secretStorageAudit{
			ProviderSecrets: credentialsPath(c.cfgDir),
			MCPTokenCache:   filepath.Join(c.cfgDir, "mcp", "mcp-*.json"),
			DashboardAuth:   dashboardAuthStorage(c.lc),
		},
		Execution: auditExecution(c, permissions.Execution.Enabled, modelBackend),
	}
	return report
}

func configuredListeners(lc *launchConfig) []listenerAudit {
	if lc == nil {
		lc = &launchConfig{}
	}
	dashboardAddr := lc.Dashboard.Addr
	if dashboardAddr == "" {
		dashboardAddr = defaultDashboardAddr
	}
	listeners := []listenerAudit{
		newListenerAudit("dashboard", lc.Dashboard.IsEnabled(), dashboardAddr,
			lc.Dashboard.TLSCert != "" && lc.Dashboard.TLSKey != "", lc.Dashboard.Auth != "", "web control panel"),
	}
	if lc.Gateway.Mode == "sse" {
		listeners = append(listeners, newListenerAudit("mcp-sse", true, lc.Gateway.SSEAddr, false, true,
			"bearer token (gateway-auth-token); plain HTTP, no TLS"))
	} else {
		listeners = append(listeners, listenerAudit{Name: "mcp-sse", Enabled: false})
	}
	if lc.Peer.Listen != "" {
		tlsEnabled := lc.Peer.Transport != "overlay"
		note := "paired-peer token authentication"
		if !tlsEnabled {
			note += "; plaintext permitted by overlay transport"
		}
		listeners = append(listeners, newListenerAudit("peer", true, lc.Peer.Listen, tlsEnabled, true, note))
	} else {
		listeners = append(listeners, listenerAudit{Name: "peer", Enabled: false})
	}
	return listeners
}

func newListenerAudit(name string, enabled bool, addr string, tlsEnabled, authEnabled bool, note string) listenerAudit {
	host := listenerHost(addr)
	return listenerAudit{
		Name: name, Enabled: enabled, Address: addr, Loopback: isLoopbackAddr(host),
		TLS: tlsEnabled, Auth: authEnabled, Note: note,
	}
}

func listenerHost(addr string) string {
	host := strings.TrimSpace(addr)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		return parsed
	}
	return host
}

func evaluateSecurityAudit(report *securityAuditReport) {
	report.Findings = nil
	add := func(id, area string, severity auditSeverity, summary, remedy string) {
		report.Findings = append(report.Findings, securityFinding{
			ID: id, Area: area, Severity: severity, Summary: summary, Remedy: remedy,
		})
	}
	for _, listener := range report.Listeners {
		if !listener.Enabled || listener.Loopback {
			continue
		}
		if !listener.TLS || !listener.Auth {
			add("listener."+listener.Name, "listeners", auditCritical,
				fmt.Sprintf("%s listens on non-loopback %q without TLS and authentication both active", listener.Name, listener.Address),
				"bind to loopback or require both TLS and authentication")
		}
	}
	if report.Browser.Enabled && report.Browser.NoSandbox {
		add("browser.no-sandbox", "browser", auditCritical,
			"browser_no_sandbox is set: Chrome will run with --no-sandbox",
			"set browser_no_sandbox=false (the secure default) unless running inside a detected container/user namespace")
	}
	if report.Web.RemoteReaderEnabled {
		add("web.remote-reader", "web", auditWarning,
			"the Jina remote reader (r.jina.ai) opt-in is enabled: target URLs may be sent to a third party after browser failures",
			"set TENANT_WEB_READER=false or clear the web skill jina_reader setting unless remote reader use is intended")
	}
	if report.RemoteMCP.TrustAnnotations && len(report.RemoteMCP.Servers) > 0 {
		add("mcp.trust-annotations", "remote_mcp", auditWarning,
			"remote MCP server annotations are trusted to classify tools as read-only",
			"leave --mcp-trust-annotations off unless every configured server is trusted")
	}
	for _, in := range report.Permissions.Integrations {
		if in.Kind != "local-mcp" {
			continue
		}
		switch {
		case in.Problem != "":
			add("mcp.local."+in.Name, "integrations", auditWarning,
				"local MCP server "+in.Name+" cannot start: "+in.Problem,
				"fix its mcp_servers entry, or install its sandbox backend")
		case in.Runs == "unconfined":
			add("mcp.local."+in.Name, "integrations", auditWarning,
				"local MCP server "+in.Name+" runs third-party code with no filesystem or network confinement (sandbox "+in.Backend+")",
				"run it under the restricted sandbox, or accept that it can read and send anything Tenant's user can")
		}
	}
	if report.RemoteMCP.AllowWrite && len(report.RemoteMCP.Servers) > 0 {
		add("mcp.write-bypass", "remote_mcp", auditCritical,
			"remote MCP write tools may run without per-action approval",
			"remove --mcp-allow-write and retain per-action approval")
	}
	for _, check := range report.Files {
		if check.Status == "warning" {
			add("file."+filepath.Base(check.Path), "file_protection", auditWarning, check.Detail,
				"restrict sensitive files to the current user")
		}
		if check.Status == "unverified" && check.Sensitive {
			add("file."+filepath.Base(check.Path), "file_protection", auditWarning, check.Detail,
				"verify the file ACL grants access only to the Tenant operator")
		}
	}
	if report.SecretStorage.DashboardAuth == "config.json (plaintext field)" {
		add("secret.dashboard-auth", "secret_storage", auditWarning,
			"the dashboard bearer token is stored in config.json rather than the protected credential store",
			"restrict config.json to the operator account and move the token to scoped secret storage in a follow-up")
	}
	if category, ok := report.Permissions.Categories[catDestructive]; ok && category.Mode == modeAllow.String() {
		add("permissions.destructive-allow", "permissions", auditCritical,
			"irreversible actions are allowed without per-action approval",
			"use the safe or balanced preset, or set destructive to ask/deny")
	}
	if category, ok := report.Permissions.Categories[catSandbox]; ok && category.Mode == modeAllow.String() {
		add("permissions.sandbox-allow", "permissions", auditCritical,
			"commands the execution sandbox refuses re-run outside it without approval, so the sandbox is advisory",
			"use the safe or balanced preset, or set sandbox to ask/deny")
	}
	// Isolation is the local surface's RESOLVED backend (auditExecution): exec
	// allowed on a restricted profile, dry run included, is not host exec.
	if category, ok := report.Permissions.Categories[catExec]; ok && category.Mode == modeAllow.String() && report.Execution.Isolation == "none" {
		add("permissions.host-exec-allow", "permissions", auditCritical,
			"host shell execution is allowed without approval and has no isolation boundary",
			"set exec to ask/deny or use an isolated execution backend")
	}
	if report.Execution.Enabled {
		executionFindings(report.Execution.Surfaces, add)
	}
	for _, surface := range report.Surfaces {
		if !surface.Active || surface.Surface == permissionSurfaceLocal {
			continue
		}
		for _, risk := range offsiteRisks {
			effective, ok := surface.Categories[risk.category]
			if !ok || effective.Mode != modeAllow.String() {
				continue
			}
			add("permissions."+surface.Surface+"."+risk.category+"-allow", "permissions", risk.severity,
				fmt.Sprintf("a %s message %s (%s: allow, from %s)", surfaceLabel(surface.Surface), risk.consequence, risk.category, effective.Source),
				offsiteRemedy(surface.Surface, risk.category, effective.Source))
		}
	}
	logbookFindings(report, add)

	report.Critical, report.Warnings = 0, 0
	for _, finding := range report.Findings {
		switch finding.Severity {
		case auditCritical:
			report.Critical++
		case auditWarning:
			report.Warnings++
		}
	}
	report.Safe = report.Critical == 0
}

// executionFindings warns, once each, about the exec surfaces whose commands
// run on the host unconfined (with why, when the built-in default fell back),
// run the sandbox in dry run, or do not run because their backend is missing.
func executionFindings(surfaces []surfaceProbe, add func(id, area string, severity auditSeverity, summary, remedy string)) {
	var host, dry, closed []string
	why := ""
	for _, s := range surfaces {
		switch {
		case s.failsClosed():
			closed = append(closed, s.Surface+" ("+s.Error+")")
		case s.unconfined():
			host = append(host, s.Surface)
			if s.Fallback != "" {
				why = ": the restricted backend can't run here: " + s.Fallback
			}
		case s.Mode != refusalMode(true):
			dry = append(dry, s.Surface)
		}
	}
	if len(host) > 0 {
		add("execution.host", "execution", auditWarning,
			"commands run directly on the host with no filesystem or network confinement ("+strings.Join(host, ", ")+")"+why,
			"use deny/ask permissions, and a restricted execution backend when one is available")
	}
	if len(dry) > 0 {
		add("execution.dry-run", "execution", auditWarning,
			"the execution sandbox is in dry run ("+strings.Join(dry, ", ")+"): what it refuses is logged, then re-run on the host, so it confines nothing yet",
			"read the sandbox log (the dashboard's sandbox tab), add the folders commands need to exec_exceptions or write_roots, then set exec_profiles.default.enforce to true")
	}
	if len(closed) > 0 {
		add("execution.unavailable", "execution", auditWarning,
			"commands do not run on "+strings.Join(closed, "; ")+": the backend is unavailable and the profile enforces",
			"install what the error names (`tenant sandbox probe`), or set another backend for the surface in exec_profiles")
	}
}

// offsiteRisks are the categories whose "allow" on an offsite surface lets a
// remote message act on this host, or send data out, with no one approving.
var offsiteRisks = []struct {
	category    string
	severity    auditSeverity
	consequence string
}{
	{catExec, auditCritical, "runs shell commands on this host without approval"},
	{catWrite, auditCritical, "writes files on this host without approval"},
	{catDestructive, auditCritical, "takes irreversible actions without approval"},
	{catSandbox, auditCritical, "re-runs commands outside the execution sandbox without approval"},
	{catSend, auditWarning, "sends email, messages or posts without approval"},
	{catEgress, auditWarning, "sends sensitive tool results across a trust boundary without approval"},
	{catMCP, auditWarning, "connects and trusts new tool servers without approval"},
}

func surfaceLabel(surface string) string {
	switch surface {
	case permissionSurfaceDiscord:
		return "Discord"
	case permissionSurfaceIMessage:
		return "iMessage"
	}
	return surface
}

// offsiteRemedy points at the setting that produced the allow.
func offsiteRemedy(surface, category, source string) string {
	if pattern, ok := strings.CutPrefix(source, "tool rule "); ok {
		return fmt.Sprintf("remove or narrow the tool rule %q (/permissions rm %s): tool rules apply on every surface", pattern, pattern)
	}
	switch surface {
	case permissionSurfaceDiscord:
		remedy := fmt.Sprintf("set it to ask or deny (/relay permissions set %s ask)", category)
		if containsString(relayExecCategories, category) {
			remedy += ", or turn off exec mode (/relay exec off)"
		}
		return remedy
	case permissionSurfaceIMessage:
		return fmt.Sprintf("set it to ask or deny (/imessage permissions set %s deny)", category)
	}
	return "set it to ask or deny"
}

// protectedFileSpotChecks checks every managed secret file (the list doctor and
// the file tools use, TEN-337), config.json (sensitive while it holds the
// dashboard token) and the agent's settings file.
func protectedFileSpotChecks(cfgDir, dataDir, agent string, configContainsSecret bool) []fileProtectionAudit {
	managed, err := managedSecrets(cfgDir, dataDir)
	checks := make([]fileProtectionAudit, 0, len(managed)+3)
	for _, secret := range managed {
		checks = append(checks, checkFileProtection(secret.path, true))
	}
	if err != nil {
		checks = append(checks, fileProtectionAudit{Path: mcpCacheDir(cfgDir), Sensitive: true, Status: "warning", Detail: err.Error()})
	}
	checks = append(checks,
		checkFileProtection(launchConfigPath(cfgDir), configContainsSecret),
		checkFileProtection(settingsPath(cfgDir, agent), false))
	return checks
}

// checkFileProtection reports path without changing it. A sensitive file must
// be owner-only as secrets.Probe verifies it (0600 in a 0700 directory on
// POSIX, a current-user-only DACL on Windows), and must not be a symlink.
func checkFileProtection(path string, sensitive bool) fileProtectionAudit {
	check := fileProtectionAudit{Path: path, Sensitive: sensitive}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		check.Status = "not_found"
		return check
	}
	if err != nil {
		check.Status = "warning"
		check.Detail = fmt.Sprintf("cannot inspect %s: %v", path, err)
		return check
	}
	if runtime.GOOS != "windows" {
		check.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
	}
	if sensitive {
		report := secrets.Probe(path)
		if !report.Protected {
			check.Status = "warning"
			check.Detail = fmt.Sprintf("%s is not owner-only: %s", path, report.Detail)
			return check
		}
		check.Status, check.Detail = "ok", report.Detail
		return check
	}
	if !info.Mode().IsRegular() {
		check.Status = "warning"
		check.Detail = path + " is not a regular file"
		return check
	}
	check.Status = "ok"
	return check
}

func dashboardAuthStorage(lc *launchConfig) string {
	if lc != nil && lc.Dashboard.Auth != "" {
		return "config.json (plaintext field)"
	}
	return "not configured"
}

func toolOverrideEnabled(overrides map[string]bool, prefix string) bool {
	for name, enabled := range overrides {
		if enabled && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func printSecurityAudit(report securityAuditReport) {
	fmt.Printf("tenant security audit — agent=%s\n", report.Agent)
	fmt.Println("\nlisteners:")
	for _, listener := range report.Listeners {
		if !listener.Enabled {
			fmt.Printf("  %-10s disabled\n", listener.Name)
			continue
		}
		fmt.Printf("  %-10s %s  loopback=%t tls=%t auth=%t\n", listener.Name, listener.Address,
			listener.Loopback, listener.TLS, listener.Auth)
	}
	fmt.Printf("\nbrowser: enabled=%t sandbox_active=%t no_sandbox=%t\n",
		report.Browser.Enabled, report.Browser.SandboxActive, report.Browser.NoSandbox)
	fmt.Printf("web: egress=%s; remote_reader=%t\n", report.Web.EgressPolicy, report.Web.RemoteReaderEnabled)
	fmt.Printf("remote MCP: servers=%d trust_annotations=%t allow_write=%t\n", len(report.RemoteMCP.Servers),
		report.RemoteMCP.TrustAnnotations, report.RemoteMCP.AllowWrite)
	fmt.Printf("execution: %s isolation=%s enabled=%t (model=%s)\n", report.Execution.Backend,
		report.Execution.Isolation, report.Execution.Enabled, report.Execution.ModelBackend)
	for _, ln := range execProfileLines(report.Execution.Surfaces) {
		fmt.Println("  " + ln)
	}
	for _, s := range report.Execution.Surfaces {
		if len(s.Exceptions) > 0 {
			fmt.Printf("  %s exceptions (exec_exceptions): %s\n", s.Surface, strings.Join(s.Exceptions, ", "))
		}
	}
	for _, surface := range report.Surfaces {
		state := "inactive"
		if surface.Active {
			state = "active"
		}
		fmt.Printf("permissions: surface=%s %s (%s) preset=%s\n", surface.Surface, state, surface.Reason, surface.Preset)
		if !surface.Active {
			continue
		}
		for _, category := range catOrder {
			if effective, ok := surface.Categories[category]; ok {
				fmt.Printf("  %-12s %-5s (%s)\n", category, effective.Mode, effective.Source)
			}
		}
	}
	if len(report.Permissions.ToolRules) > 0 {
		fmt.Printf("tool rules: %d, applied on every surface (see `tenant permissions effective`)\n", len(report.Permissions.ToolRules))
	}
	fmt.Printf("secret storage: providers=%s; mcp_tokens=%s; dashboard_auth=%s\n",
		report.SecretStorage.ProviderSecrets, report.SecretStorage.MCPTokenCache, report.SecretStorage.DashboardAuth)
	fmt.Println("file protection:")
	for _, check := range report.Files {
		fmt.Printf("  %-10s %s", check.Status, check.Path)
		if check.Mode != "" {
			fmt.Printf(" mode=%s", check.Mode)
		}
		fmt.Println()
	}
	printLogbookAudit(report.Logbook)
	fmt.Println("\nfindings:")
	if len(report.Findings) == 0 {
		fmt.Println("  none")
	}
	for _, finding := range report.Findings {
		fmt.Printf("  [%-8s] %s: %s\n", strings.ToUpper(string(finding.Severity)), finding.ID, finding.Summary)
	}
	fmt.Printf("\nsummary: %d critical, %d warning; safe=%t\n", report.Critical, report.Warnings, report.Safe)
}
