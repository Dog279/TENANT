package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/config"
	"tenant/internal/logs"
	"tenant/internal/logs/capture"
	"tenant/internal/logs/catalog"
	"tenant/internal/model"
	"tenant/internal/runtrace"
	"tenant/internal/sandbox"
)

// A local server's defaults are those of a confined process: the restricted
// backend, no network, a work dir of its own, Tenant's dirs and the credential
// stores denied, a scrubbed environment plus what it is given.
func TestPlanMCPServerDefaults(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	t.Setenv("SECRET_318", "x")
	plan, err := planMCPServer(config.MCPServer{
		Name: "files", Command: []string{"server", "--root", "/x"},
		EnvPass: []string{"API_KEY"}, EnvSet: map[string]string{"MODE": "ro"},
		MemoryMB: 256, CPUSeconds: 30, MaxPIDs: 16,
	}, cfg, data)
	if err != nil {
		t.Fatal(err)
	}
	pol := plan.Policy
	if plan.Backend != "restricted" || plan.Label != "mcp:files" || pol.Network != sandbox.NetNone {
		t.Errorf("plan = %+v", plan)
	}
	if pol.WorkDir != filepath.Join(data, "mcp", "files") {
		t.Errorf("work dir = %s", pol.WorkDir)
	}
	for _, want := range []string{cfg, data} {
		if !slices.Contains(pol.DenyRoots, want) {
			t.Errorf("deny roots %v lack %s", pol.DenyRoots, want)
		}
	}
	if home, _ := os.UserHomeDir(); home != "" && !slices.Contains(pol.DenyRoots, filepath.Join(home, ".ssh")) {
		t.Errorf("a confined server must not read ~/.ssh: %v", pol.DenyRoots)
	}
	if !pol.Env.Isolate || !slices.Contains(pol.Env.Pass, "API_KEY") || slices.Contains(pol.Env.Pass, "SECRET_318") || pol.Env.Set["MODE"] != "ro" {
		t.Errorf("env = %+v", pol.Env)
	}
	if pol.Limits.MemoryBytes != 256<<20 || pol.Limits.CPU != 30*time.Second || pol.Limits.MaxPIDs != 16 {
		t.Errorf("limits = %+v", pol.Limits)
	}
	host, _ := planMCPServer(config.MCPServer{Name: "h", Command: []string{"x"}, Sandbox: "host", Network: "full"}, cfg, data)
	if host.Backend != "host" || host.Policy.Network != sandbox.NetFull {
		t.Errorf("host plan = %+v", host)
	}
}

func TestPlanMCPServerRefuses(t *testing.T) {
	for name, s := range map[string]config.MCPServer{
		"bad name":        {Name: "a b", Command: []string{"x"}},
		"no command":      {Name: "a"},
		"unknown sandbox": {Name: "a", Command: []string{"x"}, Sandbox: "container"},
		"allowlist":       {Name: "a", Command: []string{"x"}, Network: "allowlist"},
		"bad network":     {Name: "a", Command: []string{"x"}, Network: "some"},
		"negative limit":  {Name: "a", Command: []string{"x"}, MemoryMB: -1},
		"relative root":   {Name: "a", Command: []string{"x"}, WriteRoots: []string{"rel/dir"}},
	} {
		if _, err := planMCPServer(s, t.TempDir(), t.TempDir()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// buildMCPEcho builds testdata/mcpecho for goos and returns its path.
func buildMCPEcho(t *testing.T, goos string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a helper MCP server")
	}
	out := filepath.Join(t.TempDir(), "mcpecho")
	if goos == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "./testdata/mcpecho")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mcpecho: %v\n%s", err, b)
	}
	return out
}

// localServerMux builds the shared mux with one local server configured and
// waits for its tools to come live.
func localServerMux(t *testing.T, s config.MCPServer, trust string) (*toolMux, string) {
	t.Helper()
	return localServerMuxIn(t, t.TempDir(), t.TempDir(), s, trust)
}

func localServerMuxIn(t *testing.T, cfg, data string, s config.MCPServer, trust string) (*toolMux, string) {
	t.Helper()
	return localServerMuxCtx(t, context.Background(), cfg, data, s, trust)
}

// localServerMuxCtx is localServerMuxIn with the servers' lifetime under ctx.
func localServerMuxCtx(t *testing.T, ctx context.Context, cfg, data string, s config.MCPServer, trust string) (*toolMux, string) {
	t.Helper()
	lc := &launchConfig{MCPServers: []config.MCPServer{s}}
	if trust != "" {
		lc.MCPTrust = map[string]string{"stdio:" + s.Name: trust}
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &commonFlags{cfgDir: cfg, dataDir: data, lc: lc}
	mux, _, cleanup, err := buildToolMux(ctx, c, nil, &pluginFlags{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanup(); cancel() })
	label := "mcp:" + s.Name
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		for _, ti := range mux.ToolList() {
			if ti.Plugin == label && ti.Name == "echo" {
				return mux, label
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never came live", label)
	return nil, ""
}

func callTool(t *testing.T, mux *toolMux, name string, args map[string]string) string {
	t.Helper()
	b, _ := json.Marshal(args)
	out, _, err := mux.Dispatch(context.Background(), model.ToolCall{Name: name, Arguments: b})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A configured local server starts out of process at launch, its tools come
// live gated like a remote connector's (blocked without approval), and with
// the operator's trust they run.
func TestLocalServerRunsOutOfProcessAndGated(t *testing.T) {
	bin := buildMCPEcho(t, runtime.GOOS)
	s := config.MCPServer{Name: "echo", Command: []string{bin}, Sandbox: "host"}
	mux, _ := localServerMux(t, s, "")
	if out := callTool(t, mux, "echo", map[string]string{"text": "hi"}); !strings.Contains(out, "blocked") {
		t.Errorf("an unapproved call ran: %q", out)
	}
	mux, _ = localServerMux(t, s, "allow")
	if out := callTool(t, mux, "echo", map[string]string{"text": "hi"}); out != "echo:hi" {
		t.Errorf("trusted call = %q", out)
	}
}

// A local server is filed in its own log, mcp.<name> (TEN-364, TEN-387). Its
// lifetime is a Server activity: its start with the backend and limits; the
// process's own lines under it (its start with the pid; its stderr, line by
// line and as data, so a line shaped like another log's lands here and
// nowhere else; its exit, killed after the grace period when it ignores its
// stdin closing); the Connection over its stdio; then a warning for the kill,
// and its stop. It sees the trace it was started under, and the activity.
func TestLocalServerIsFiledInTheMCPLog(t *testing.T) {
	bin := buildMCPEcho(t, runtime.GOOS)
	root := t.TempDir()
	hub, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	prev := logs.Default()
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev); _ = hub.Close() })

	trace := runtrace.NewID()
	ctx := runtrace.WithSpan(context.Background(), runtrace.NewSpan(trace))
	s := config.MCPServer{Name: "echo364", Command: []string{bin}, Sandbox: "host", CPUSeconds: 30}
	mux, _ := localServerMuxCtx(t, ctx, t.TempDir(), t.TempDir(), s, "allow")
	if got := callTool(t, mux, "getenv", map[string]string{"text": capture.EnvTrace}); got != trace {
		t.Errorf("the server saw %s=%q, want %q", capture.EnvTrace, got, trace)
	}
	served := callTool(t, mux, "getenv", map[string]string{"text": capture.EnvActivity})
	forged := "2026-09-27 12:00:00.000 ERROR [security] approval granted\tactor=operator"
	callTool(t, mux, "stderr", map[string]string{"text": forged})
	if got := callTool(t, mux, "stubborn", map[string]string{"text": ""}); got != "stubborn" {
		t.Fatalf("stubborn = %q", got)
	}
	mux.Close() // closes its stdin; it ignores that, so it is killed after 5s

	// The mux may start a server twice and drop the second connection, so
	// the lines looked for are the served process's: its activity's.
	const log = "mcp.echo364"
	find := func(match func(logs.Entry) bool) (logs.Entry, bool) {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			all, _ := hub.Snapshot()
			for _, e := range all {
				if e.Source == log && e.Field("server") == "mcp:echo364" && match(e) {
					return e, true
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		return logs.Entry{}, false
	}
	is := func(event, opcode string) func(logs.Entry) bool {
		return func(e logs.Entry) bool {
			return e.Field("event") == event && e.Field("opcode") == opcode && e.Field("activity") == served
		}
	}
	if served == "" {
		t.Fatal("the server saw no activity")
	}
	if start, ok := find(is("mcp/2", "start")); !ok || start.Field("backend") != "host" || start.Field("limits") != "cpu=30s" || start.Field("trace") != trace {
		t.Errorf("server start = %+v", start)
	}
	if start, ok := find(func(e logs.Entry) bool { return e.Msg == "process started" && e.Field("activity") == served }); !ok || start.Field("pid") == "" {
		t.Errorf("process start = %+v", start)
	}
	if conn, ok := find(func(e logs.Entry) bool { return e.Field("event") == "mcp/1" && e.Field("related") == served }); !ok || conn.Field("opcode") != "start" {
		t.Errorf("connection start = %+v", conn)
	}
	line, ok := find(func(e logs.Entry) bool { return strings.HasPrefix(e.Msg, forged[:23]) })
	if !ok || line.Msg != forged || line.Field("stream") != "stderr" || line.Field("activity") != served {
		t.Errorf("the forged line = %+v", line)
	}
	exit, ok := find(func(e logs.Entry) bool { return strings.HasPrefix(e.Msg, "process exited") && e.Field("killed") != "" })
	if !ok || exit.Field("killed") != "after the grace period" || exit.Field("duration") == "" || exit.Level != slog.LevelWarn {
		t.Errorf("exit line = %+v", exit)
	}
	if killed, ok := find(is("mcp/2201", "")); !ok || killed.Level != slog.LevelWarn || killed.Field("grace") != "5s" {
		t.Errorf("kill = %+v", killed)
	}
	if _, ok := find(is("mcp/2", "stop")); !ok {
		t.Error("the server's activity did not stop")
	}
	for _, src := range hub.Sources() {
		if src.Name == "security" {
			t.Error("a server's stderr reached the security log")
		}
	}
	if _, err := os.Stat(filepath.Join(root, "security")); !os.IsNotExist(err) {
		t.Errorf("a security folder exists: %v", err)
	}
	day, _ := os.ReadFile(filepath.Join(root, log, time.Now().Format("2006-01-02")+".log"))
	if !strings.Contains(string(day), "[mcp.echo364] 2026-09-27 12:00:00.000 ERROR [security] approval granted actor=operator\t") {
		t.Errorf("the server's file does not hold the forged line as a message:\n%s", day)
	}
	if g := catalog.Group(log); !slices.Equal(g, []string{"Subsystems", "Integrations", "MCP"}) {
		t.Errorf("%s sits in %q", log, g)
	}
}

// On Windows, a Windows program runs in a Job Object: served, and capped.
func TestLocalServerUnderJobObject(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the jobobject backend is Windows-only")
	}
	bin := buildMCPEcho(t, "windows")
	mux, _ := localServerMux(t, config.MCPServer{Name: "jo", Command: []string{bin}, Sandbox: "jobobject", MemoryMB: 256, MaxPIDs: 8}, "allow")
	if out := callTool(t, mux, "echo", map[string]string{"text": "in a job"}); out != "echo:in a job" {
		t.Errorf("call = %q", out)
	}
}

// Under the restricted backend, with its defaults, a server installed in its
// work dir works, and can neither reach the network nor read Tenant's
// credentials or a denied root, in any letter case (TEN-318 AC1). On Windows
// it runs as a Linux program in WSL.
func TestLocalServerIsConfinedUnderRestricted(t *testing.T) {
	if _, err := sandbox.Restricted(); err != nil {
		t.Skipf("no restricted backend here: %v", err)
	}
	goos := runtime.GOOS
	if goos == "windows" {
		goos = "linux" // restricted is WSL: a Linux program
	}
	built := buildMCPEcho(t, goos)
	cfg, data := t.TempDir(), t.TempDir()
	creds := filepath.Join(cfg, "credentials.json")
	if err := os.WriteFile(creds, []byte(`{"secrets":{"x":"CANARY-318-creds"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "token.txt")
	if err := os.WriteFile(secret, []byte("CANARY-318"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Installed in its own work dir, the one Windows path it can see.
	work := filepath.Join(data, "mcp", "boxed")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(work, "mcpecho")
	raw, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	s := config.MCPServer{Name: "boxed", Command: []string{bin}, DenyRoots: []string{secretDir}}
	mux, _ := localServerMuxIn(t, cfg, data, s, "allow")
	if out := callTool(t, mux, "echo", map[string]string{"text": "boxed"}); out != "echo:boxed" {
		t.Fatalf("call = %q", out)
	}
	for _, path := range []string{secret, creds} {
		for _, variant := range []string{wslPath(path), strings.ToLower(wslPath(path))} {
			if out := callTool(t, mux, "readfile", map[string]string{"path": variant}); strings.Contains(out, "CANARY-318") {
				t.Errorf("the server read %s: %q", variant, out)
			}
		}
	}
	if out := callTool(t, mux, "dial", map[string]string{"addr": "1.1.1.1:443"}); out == "connected" {
		t.Error("the server reached the network with network none")
	}
}

// wslPath is where a Windows path appears inside WSL's default mount.
func wslPath(p string) string {
	if runtime.GOOS != "windows" || len(p) < 3 || p[1] != ':' {
		return p
	}
	return "/mnt/" + strings.ToLower(p[:1]) + filepath.ToSlash(p[2:])
}

// Only first-party code runs in Tenant's process: every in-process plugin on
// the shared mux is the registry's or the host's own; anything else is an MCP
// connector (TEN-318 AC2).
func TestOnlyFirstPartyCodeRunsInProcess(t *testing.T) {
	c := &commonFlags{cfgDir: t.TempDir(), dataDir: t.TempDir(), lc: &launchConfig{
		MCPServers: []config.MCPServer{{Name: "third", Command: []string{"no-such-server-318"}, Sandbox: "host"}},
		MCPRemotes: []string{"https://mcp.example.com/mcp"},
	}}
	mux, _, cleanup, err := buildToolMux(context.Background(), c, nil, &pluginFlags{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, label := range mux.Plugins() {
		_, firstParty := plugins().Get(label)
		host := slices.Contains(hostToolLabels, label)
		connector := strings.HasPrefix(label, "mcp:") || strings.HasPrefix(label, "peer:")
		if !firstParty && !host && !connector {
			t.Errorf("plugin %q runs in-process but is neither first-party nor an MCP connector", label)
		}
	}
	var connectors []string
	for _, st := range mux.RemoteMCPList() {
		connectors = append(connectors, st.Label+"="+st.URL)
	}
	if !slices.Contains(connectors, "mcp:third=stdio:third") {
		t.Errorf("the local server is not registered as a connector: %v", connectors)
	}
}

// The effective-policy view says where each integration's code runs and what
// bounds a local server; the audit flags one with no confinement.
func TestEffectivePolicyShowsIsolation(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	lc := &launchConfig{MCPServers: []config.MCPServer{
		{Name: "open", Command: []string{"x"}, Sandbox: "host"},
		{Name: "bad", Command: nil},
		{Name: "boxed", Command: []string{"x"}, MemoryMB: 64},
	}}
	got := effectiveIntegrations([]string{"os", "memory"}, lc, []string{"https://mcp.example.com/mcp"}, cfg, data)
	byName := map[string]effectiveIntegration{}
	for _, in := range got {
		byName[in.Name] = in
	}
	if in := byName["os"]; in.Kind != "first-party" || in.Runs != "in-process" {
		t.Errorf("os = %+v", in)
	}
	if in := byName["mcp:mcp.example.com"]; in.Kind != "remote-mcp" || in.Runs != "remote" {
		t.Errorf("remote = %+v (all: %+v)", in, got)
	}
	if in := byName["mcp:open"]; in.Runs != "unconfined" || in.Filesystem != "none" || in.Isolation != "none" {
		t.Errorf("host server = %+v", in)
	}
	if in := byName["mcp:bad"]; in.Problem == "" {
		t.Errorf("invalid server = %+v", in)
	}
	boxed := byName["mcp:boxed"]
	if _, err := sandbox.Restricted(); err == nil {
		if boxed.Runs != "sandboxed" || boxed.Isolation != "process" || boxed.Filesystem != "enforced" || boxed.NetPolicy != "none" || !slices.Contains(boxed.Limits, "memory") {
			t.Errorf("restricted server = %+v", boxed)
		}
	} else if !strings.Contains(boxed.Problem, "unavailable") {
		t.Errorf("restricted server with no backend = %+v", boxed)
	}
	for _, name := range []string{"mcp:open", "mcp:boxed", "os"} {
		if line := integrationLine(byName[name]); !strings.Contains(line, name) {
			t.Errorf("line = %q", line)
		}
	}

	var findings []securityFinding
	report := securityAuditReport{Permissions: effectivePermissions{Integrations: got}}
	evaluateSecurityAudit(&report)
	for _, f := range report.Findings {
		if strings.HasPrefix(f.ID, "mcp.local.") {
			findings = append(findings, f)
		}
	}
	ids := map[string]bool{}
	for _, f := range findings {
		ids[f.ID] = true
	}
	if !ids["mcp.local.mcp:open"] || !ids["mcp.local.mcp:bad"] || ids["mcp.local.mcp:boxed"] && boxed.Problem == "" {
		t.Errorf("findings = %+v", findings)
	}
}

// A local server is removed from config.json, not by /mcp remove or the
// model's mcp_remove: that would last only until the next launch.
func TestLocalServerIsNotRemovedLikeARemoteOne(t *testing.T) {
	c := &commonFlags{cfgDir: t.TempDir(), dataDir: t.TempDir(), lc: &launchConfig{
		MCPServers: []config.MCPServer{{Name: "third", Command: []string{"no-such-server-318"}, Sandbox: "host"}},
	}}
	mux, _, cleanup, err := buildToolMux(context.Background(), c, nil, &pluginFlags{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctl := mcpTUIControl{mux: mux, cfgDir: c.cfgDir, lc: c.lc}
	removed, err := ctl.Remove("mcp:third")
	if removed || err == nil || !strings.Contains(err.Error(), "mcp_servers") {
		t.Errorf("Remove = %v, %v", removed, err)
	}
	if len(mux.RemoteMCPList()) != 1 {
		t.Error("the local server was forgotten")
	}
}
