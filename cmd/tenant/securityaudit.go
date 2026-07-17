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

	"tenant/internal/plugins/web"
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

type executionAudit struct {
	Backend        string `json:"backend"`
	Isolation      string `json:"isolation"`
	Enabled        bool   `json:"enabled"`
	ModelBackend   string `json:"model_backend"`
	FilesystemRoot string `json:"filesystem_root"`
}

type securityAuditReport struct {
	Agent         string                `json:"agent"`
	Listeners     []listenerAudit       `json:"listeners"`
	Browser       browserAudit          `json:"browser"`
	Web           webAudit              `json:"web"`
	Permissions   effectivePermissions  `json:"permissions"`
	RemoteMCP     remoteMCPAudit        `json:"remote_mcp"`
	Files         []fileProtectionAudit `json:"file_protection"`
	SecretStorage secretStorageAudit    `json:"secret_storage"`
	Execution     executionAudit        `json:"execution"`
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

func cmdSecurityAudit(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("security audit", flag.ContinueOnError)
	c := bindCommon(fs)
	pf := bindPluginFlags(fs)
	asJSON := fs.Bool("json", false, "machine-readable output (for CI)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	applyPluginConfig(c, pf)
	permissions, err := composeEffectivePermissions(c, pf, permissionSurfaceLocal)
	if err != nil {
		return fmt.Errorf("compose permissions: %w", err)
	}
	report := buildSecurityAuditReport(c, pf, permissions)
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

func buildSecurityAuditReport(c *commonFlags, pf *pluginFlags, permissions effectivePermissions) securityAuditReport {
	webEnabled := pf.web || toolOverrideEnabled(permissions.ToolOverrides, "web_")
	readerEnabled := webEnabled && !readerDisabled()
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
			Enabled: webEnabled, NoSandbox: web.BrowserNoSandbox,
			SandboxActive: webEnabled && !web.BrowserNoSandbox,
		},
		Web: webAudit{
			Enabled: webEnabled, EgressPolicy: "unrestricted (no private-network destination guard)",
			RemoteReader: "Jina Reader (r.jina.ai)", RemoteReaderEnabled: readerEnabled,
		},
		Permissions: permissions,
		RemoteMCP: remoteMCPAudit{
			Servers: servers, TrustAnnotations: pf.mcpTrustAnnotations, AllowWrite: pf.mcpAllowWrite,
		},
		Files: protectedFileSpotChecks(c.cfgDir, c.agent, c.lc.Dashboard.Auth != ""),
		SecretStorage: secretStorageAudit{
			ProviderSecrets: credentialsPath(c.cfgDir),
			MCPTokenCache:   filepath.Join(c.cfgDir, "mcp", "mcp-*.json"),
			DashboardAuth:   dashboardAuthStorage(c.lc),
		},
		Execution: executionAudit{
			Backend: "host shell", Isolation: "none", Enabled: permissions.Execution.Enabled,
			ModelBackend: modelBackend, FilesystemRoot: "unrestricted host filesystem",
		},
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
		newListenerAudit("dashboard", lc.Dashboard.dashboardEnabled(), dashboardAddr,
			lc.Dashboard.TLSCert != "" && lc.Dashboard.TLSKey != "", lc.Dashboard.Auth != "", "web control panel"),
	}
	if lc.Gateway.Mode == "sse" || lc.Gateway.Mode == "both" {
		listeners = append(listeners, newListenerAudit("mcp-sse", true, lc.Gateway.SSEAddr, false, false,
			"legacy SSE transport has no auth"))
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
			"the web browser is enabled and Chrome will run with --no-sandbox",
			"disable the web tool until SEC-003 restores the Chrome sandbox")
	}
	if report.Web.Enabled {
		add("web.unrestricted-egress", "web", auditWarning,
			"web navigation has no private-network destination guard", "restrict web egress and block local/private destinations")
	}
	if report.Web.RemoteReaderEnabled {
		add("web.remote-reader", "web", auditCritical,
			"Jina Reader is enabled and may receive requested URLs after browser failures",
			"set TENANT_WEB_NO_READER=1 until remote reader use is explicitly approved")
	}
	if report.RemoteMCP.TrustAnnotations && len(report.RemoteMCP.Servers) > 0 {
		add("mcp.trust-annotations", "remote_mcp", auditWarning,
			"remote MCP server annotations are trusted to classify tools as read-only",
			"leave --mcp-trust-annotations off unless every configured server is trusted")
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
	if category, ok := report.Permissions.Categories[catExec]; ok && category.Mode == modeAllow.String() && report.Execution.Isolation == "none" {
		add("permissions.host-exec-allow", "permissions", auditCritical,
			"host shell execution is allowed without approval and has no isolation boundary",
			"set exec to ask/deny or use an isolated execution backend")
	}
	if report.Execution.Enabled && report.Execution.Isolation == "none" {
		add("execution.host", "execution", auditWarning,
			"shell execution is enabled directly on the host without a container or filesystem root",
			"use deny/ask permissions and a restricted execution backend when one is available")
	}

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

func protectedFileSpotChecks(cfgDir, agent string, configContainsSecret bool) []fileProtectionAudit {
	type target struct {
		path      string
		sensitive bool
	}
	targets := []target{
		{credentialsPath(cfgDir), true},
		{launchConfigPath(cfgDir), configContainsSecret},
		{settingsPath(cfgDir, agent), false},
	}
	if matches, err := filepath.Glob(filepath.Join(cfgDir, "mcp", "mcp-*.json")); err == nil {
		for _, match := range matches {
			targets = append(targets, target{match, true})
		}
	}
	checks := make([]fileProtectionAudit, 0, len(targets))
	for _, target := range targets {
		checks = append(checks, checkFileProtection(target.path, target.sensitive))
	}
	return checks
}

func checkFileProtection(path string, sensitive bool) fileProtectionAudit {
	check := fileProtectionAudit{Path: path, Sensitive: sensitive}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		check.Status = "not_found"
		return check
	}
	if err != nil {
		check.Status = "warning"
		check.Detail = fmt.Sprintf("cannot inspect %s: %v", path, err)
		return check
	}
	check.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
	if !info.Mode().IsRegular() {
		check.Status = "warning"
		check.Detail = path + " is not a regular file"
		return check
	}
	if sensitive && runtime.GOOS == "windows" {
		check.Status = "unverified"
		check.Detail = path + " exists; Windows ACL protection was not verified"
		return check
	}
	if sensitive && info.Mode().Perm()&0o077 != 0 {
		check.Status = "warning"
		check.Detail = fmt.Sprintf("%s has group/world permission bits %s", path, check.Mode)
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
	fmt.Printf("permissions: surface=%s preset=%s\n", report.Permissions.Surface, report.Permissions.Preset)
	for _, category := range catOrder {
		if effective, ok := report.Permissions.Categories[category]; ok {
			fmt.Printf("  %-12s %-5s (%s)\n", category, effective.Mode, effective.Source)
		}
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
	fmt.Println("\nfindings:")
	if len(report.Findings) == 0 {
		fmt.Println("  none")
	}
	for _, finding := range report.Findings {
		fmt.Printf("  [%-8s] %s: %s\n", strings.ToUpper(string(finding.Severity)), finding.ID, finding.Summary)
	}
	fmt.Printf("\nsummary: %d critical, %d warning; safe=%t\n", report.Critical, report.Warnings, report.Safe)
}
