package main

// mcpstdio.go runs the local MCP servers config.json lists under mcp_servers
// (TEN-318). A server is third-party code, so it never runs in Tenant's
// process: it runs out of process under the execution sandbox — the
// restricted backend unless the operator names another — and speaks MCP over
// its stdin and stdout. Its tools are gated like a remote connector's (trust,
// tool rules, the approval gate), and `tenant permissions effective` shows
// where it runs and what bounds it.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"tenant/internal/config"
	"tenant/internal/logs/capture"
	"tenant/internal/logs/event"
	"tenant/internal/plugins/mcpremote"
	"tenant/internal/sandbox"
	"tenant/internal/toolmux"
)

// mcpServerNames are the names a local server may have: its connector label
// is mcp:<name>.
var mcpServerNames = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// mcpServerBackends are the sandbox backends a local server may run under.
var mcpServerBackends = []string{"restricted", "jobobject", "host"}

// mcpServerPlan is a validated local server: its backend and its boundary.
type mcpServerPlan struct {
	Server  config.MCPServer
	Label   string
	Backend string
	Policy  sandbox.Policy
}

// planMCPServer validates a server and builds its sandbox policy. The
// defaults are those of a confined process:
//   - the restricted backend;
//   - no network;
//   - a work dir of its own, <data>/mcp/<name>;
//   - Tenant's config and data dirs, its binary and (confined) the local
//     credential stores denied;
//   - a scrubbed environment plus what env_pass and env_set add.
func planMCPServer(s config.MCPServer, cfgDir, dataDir string) (mcpServerPlan, error) {
	if !mcpServerNames.MatchString(s.Name) {
		return mcpServerPlan{}, fmt.Errorf("mcp_servers: name %q must be letters, digits, '-' or '_' (at most 64)", s.Name)
	}
	where := "mcp_servers." + s.Name
	if len(s.Command) == 0 || strings.TrimSpace(s.Command[0]) == "" {
		return mcpServerPlan{}, fmt.Errorf("%s: command is required", where)
	}
	backend := s.Sandbox
	if backend == "" {
		backend = "restricted"
	}
	if !containsString(mcpServerBackends, backend) {
		return mcpServerPlan{}, fmt.Errorf("%s: unknown sandbox %q (want %s)", where, s.Sandbox, strings.Join(mcpServerBackends, ", "))
	}
	netName := s.Network
	if netName == "" {
		netName = "none"
	}
	network, ok := sandbox.ParseNetworkMode(netName)
	if !ok || network == sandbox.NetAllowlist {
		return mcpServerPlan{}, fmt.Errorf("%s: network %q (want none, full or inherit; no backend enforces an allowlist yet)", where, s.Network)
	}
	if s.CPUSeconds < 0 || s.MemoryMB < 0 || s.MaxPIDs < 0 {
		return mcpServerPlan{}, fmt.Errorf("%s: limits must not be negative", where)
	}
	for _, group := range [][]string{s.ReadRoots, s.WriteRoots, s.DenyRoots, {s.WorkDir}} {
		for _, r := range group {
			if r != "" && !filepath.IsAbs(expandHome(r)) && !strings.HasPrefix(r, "/") {
				return mcpServerPlan{}, fmt.Errorf("%s: path %q must be absolute (or start with ~)", where, r)
			}
		}
	}
	workDir := expandHome(s.WorkDir)
	if workDir == "" {
		workDir = filepath.Join(dataDir, "mcp", s.Name)
	}
	env := scrubbedEnvPolicy()
	env.Pass = append(env.Pass, s.EnvPass...)
	env.Set = s.EnvSet
	pol := sandbox.Policy{
		WorkDir:    workDir,
		ReadRoots:  expandRoots(s.ReadRoots),
		WriteRoots: expandRoots(s.WriteRoots),
		DenyRoots:  tenantDenyRoots(cfgDir, dataDir, expandRoots(s.DenyRoots)...),
		Network:    network,
		Env:        env,
		Limits: sandbox.Limits{
			CPU:         time.Duration(s.CPUSeconds) * time.Second,
			MemoryBytes: int64(s.MemoryMB) << 20,
			MaxPIDs:     s.MaxPIDs,
		},
		Enforce: true,
	}
	if backend != "host" {
		pol.DenyRoots = dedupeNonEmpty(append(pol.DenyRoots, credentialStoreRoots()...)...)
	}
	return mcpServerPlan{Server: s, Label: "mcp:" + s.Name, Backend: backend, Policy: pol}, nil
}

// mcpStdioOpener starts plan's server under its sandbox and connects to it.
// The process lives as long as the context the mux passes (the command's).
// Its lifetime is a Server activity in its log, mcp.<name> (logging v2
// §8.4): the process's own lines (its start with the pid, its stderr, line
// by line, its exit) and the Connection over its stdio belong to it, and its
// environment carries it. The stderr tail it keeps still explains a server
// that fails to start.
func mcpStdioOpener(plan mcpServerPlan) toolmux.StdioOpener {
	return func(ctx context.Context, trustAnnotations bool, policy mcpremote.Policy) (_ plugin, _ func(), err error) {
		ctx, act := event.Start(mcpremote.WithServerLog(ctx, plan.Label), mcpremote.Server, "server", plan.Label,
			"backend", plan.Backend, "limits", plan.Policy.Limits.String(), "command", clipLog(strings.Join(plan.Server.Command, " ")))
		var proc *sandbox.Process
		defer func() {
			if proc == nil {
				act.End(&err) // it never ran; once it has, its exit ends the activity
			}
		}()
		b, err := sandbox.For(plan.Backend)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: its %s sandbox is not available here (%v); install it, or set \"sandbox\" for this server in mcp_servers", plan.Label, plan.Backend, err)
		}
		if err := os.MkdirAll(plan.Policy.WorkDir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("%s: work dir: %w", plan.Label, err)
		}
		d, cleanup, err := mcpremote.OpenStdio(ctx, mcpremote.StdioConfig{
			Label: plan.Label,
			Connect: func(ctx context.Context) (io.ReadCloser, io.WriteCloser, error) {
				ch := capture.New(ctx, mcpremote.ServerLog(plan.Label), "server", plan.Label)
				pol := plan.Policy
				pol.Env.Add, pol.Stderr = ch.Env(), ch
				p, err := sandbox.Start(ctx, b, plan.Server.Command, pol)
				if err != nil {
					ch.Exited(err, "backend", plan.Backend)
					return nil, nil, err
				}
				ch.Started("pid", p.Pid(), "backend", plan.Backend, "limits", pol.Limits.String(),
					"command", clipLog(strings.Join(plan.Server.Command, " ")))
				go func() {
					err := p.Wait()
					kv := []any{"pid", p.Pid(), "backend", plan.Backend}
					if p.Killed() {
						kv = append(kv, "killed", "after the grace period")
					}
					ch.Exited(err, kv...)
					switch {
					case p.Killed():
						mcpremote.ServerKilled.Log(ctx, "server", plan.Label, "pid", p.Pid(), "grace", serverGrace)
						err = nil // Tenant stopped it
					case ctx.Err() != nil:
						err = nil // Tenant stopped it: a shutdown
					}
					act.End(&err)
				}()
				proc = p
				return p.Stdout(), processStdin{p}, nil
			},
		}, trustAnnotations, policy)
		if err != nil {
			if proc != nil {
				if tail := strings.TrimSpace(proc.Stderr()); tail != "" {
					err = fmt.Errorf("%w; the server wrote: %s", err, clipLog(tail))
				}
			}
			return nil, nil, err
		}
		return d, cleanup, nil
	}
}

// serverGrace is how long a local server has to exit once its stdin closes.
const serverGrace = 5 * time.Second

// processStdin is a server's stdin whose Close ends it the way the stdio
// transport says to: close its input, wait, then kill it.
type processStdin struct{ p *sandbox.Process }

func (s processStdin) Write(b []byte) (int, error) { return s.p.Stdin().Write(b) }
func (s processStdin) Close() error                { return s.p.Close(serverGrace) }

// registerMCPServers registers every configured local server as a connector
// and starts it in the background, as remote connectors reconnect at launch.
// A server whose config is invalid is skipped with a warning in its log.
func registerMCPServers(mux *toolMux, lc *launchConfig, cfgDir, dataDir string) {
	if lc == nil {
		return
	}
	for _, s := range lc.MCPServers {
		label := "mcp:" + s.Name
		plan, err := planMCPServer(s, cfgDir, dataDir)
		if err != nil {
			mcpremote.ServerSkipped.Log(mcpremote.WithServerLog(context.Background(), label), "server", label, "err", err)
			continue
		}
		open := mcpStdioOpener(plan)
		hint := fmt.Sprintf("`/enable %s` starts `%s` under the %s sandbox", plan.Label, strings.Join(s.Command, " "), plan.Backend)
		label, err = mux.RegisterMCPStdio(s.Name, hint, open)
		if err != nil {
			mcpremote.ServerSkipped.Log(mcpremote.WithServerLog(context.Background(), plan.Label), "server", plan.Label, "err", err)
			continue
		}
		go mux.StartMCPStdioSilently(label, open)
	}
}

// effectiveIntegration is one integration and where its code runs (TEN-318):
// a first-party plugin in Tenant's process; an MCP server out of it, remote
// over HTTP or local under the sandbox, with what that sandbox enforces here.
type effectiveIntegration struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"` // first-party | remote-mcp | local-mcp
	Runs       string   `json:"runs"` // in-process | remote | sandboxed | unconfined
	Backend    string   `json:"backend,omitempty"`
	Isolation  string   `json:"isolation,omitempty"`
	Filesystem string   `json:"filesystem,omitempty"` // enforced | partial | none
	Network    string   `json:"network,omitempty"`
	NetPolicy  string   `json:"network_policy,omitempty"`
	Env        string   `json:"env,omitempty"`
	Limits     []string `json:"limits,omitempty"` // the requested caps this backend applies
	Problem    string   `json:"problem,omitempty"`
}

// effectiveIntegrations lists the configured integrations: the first-party
// plugins among tools, then remote and local MCP servers.
func effectiveIntegrations(tools []string, lc *launchConfig, remotes []string, cfgDir, dataDir string) []effectiveIntegration {
	var out []effectiveIntegration
	for _, d := range plugins().All() {
		if containsString(tools, d.ID) {
			out = append(out, effectiveIntegration{Name: d.ID, Kind: "first-party", Runs: "in-process"})
		}
	}
	for _, url := range remotes {
		out = append(out, effectiveIntegration{Name: mcpLabel(url), Kind: "remote-mcp", Runs: "remote", Isolation: "separate host"})
	}
	if lc == nil {
		return out
	}
	for _, s := range lc.MCPServers {
		in := effectiveIntegration{Name: "mcp:" + s.Name, Kind: "local-mcp", Runs: "sandboxed"}
		plan, err := planMCPServer(s, cfgDir, dataDir)
		if err != nil {
			in.Problem = err.Error()
			out = append(out, in)
			continue
		}
		in.Backend, in.NetPolicy = plan.Backend, plan.Policy.Network.String()
		b, err := sandbox.For(plan.Backend)
		if err != nil {
			in.Problem = "sandbox unavailable: " + err.Error()
			out = append(out, in)
			continue
		}
		caps := b.Probe(context.Background())
		in.Isolation, in.Filesystem, in.Network, in.Env = caps.Isolation.String(), caps.Filesystem.String(), caps.Network.String(), caps.Env.String()
		if caps.Filesystem == sandbox.EnforceNone && caps.Network == sandbox.EnforceNone {
			in.Runs = "unconfined"
		}
		requested := map[string]bool{"memory": s.MemoryMB > 0, "cpu": s.CPUSeconds > 0, "processes": s.MaxPIDs > 0}
		for _, l := range sandbox.StartLimits(b) {
			if requested[l] {
				in.Limits = append(in.Limits, l)
			}
		}
		out = append(out, in)
	}
	return out
}

// integrationLine is one integration in `tenant permissions effective`.
func integrationLine(in effectiveIntegration) string {
	switch in.Kind {
	case "first-party":
		return fmt.Sprintf("%s — first-party, in Tenant's process", in.Name)
	case "remote-mcp":
		return fmt.Sprintf("%s — remote MCP server, out of process on another host", in.Name)
	}
	if in.Problem != "" {
		return fmt.Sprintf("%s — local MCP server: %s", in.Name, in.Problem)
	}
	limits := "none"
	if len(in.Limits) > 0 {
		limits = strings.Join(in.Limits, ", ")
	}
	return fmt.Sprintf("%s — local MCP server, out of process under %s (isolation %s): filesystem confinement %s; network %s, enforcement %s; env allowlist %s; caps: %s",
		in.Name, in.Backend, in.Isolation, in.Filesystem, in.NetPolicy, in.Network, in.Env, limits)
}
