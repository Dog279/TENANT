package main

// childmux.go (TEN-531): the tool mux of a fleet child. A child serves the
// tools that act on its own machine and nothing that needs an agent or its
// master's state, so it does not use buildToolMux, which opens the memory
// stores and registers memory_search, memory_history and ui_render:
//
//   - the OS tools and the browser, always;
//   - SQL, the wiki and iMessage, once the child's own config.json (in its
//     directory, with the keys a full Tenant reads) configures them;
//   - the local MCP servers that config.json lists.
//
// Not here: helpers, teams, research, goals, cron, memory, peers, remote MCP
// connectors, the cloud integrations, and any fleet_* tool, so a call cannot
// go on from one child to another.
//
// Every plugin's own allow switch is off and its Confirm is the child's gate
// (fleet.Gate): each gated action is checked against this machine's ceiling
// and then decided on the master. No command is judged read-only here: the
// classifier that lets a master's own commands run unasked (osys.ReadOnly)
// is consulted by a master's approval broker, and a child has none.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/model"
	"tenant/internal/plugins/osys"
	"tenant/internal/plugins/web"
	"tenant/internal/sandbox"
)

// surfaceChild names a child's commands in its own logs: they come from the
// master, over the link.
const surfaceChild = "fleet"

// childDataDir is where the child in dir keeps what it works with: the logs
// of its background commands, screenshots, the wiki's index, a local MCP
// server's work dir. It is beside the child's directory, not in it, because
// the whole directory is closed to the file tools and these are not secrets.
func childDataDir(dir string) string { return filepath.Clean(dir) + "-data" }

// childMemoryDir holds the copies of the machine's memory its master sent
// (TEN-532): inside the child's own directory, which the file tools refuse.
func childMemoryDir(dir string) string { return filepath.Join(dir, "memory") }

// buildChildMux builds the tools the child in dir serves. confirm is the
// approval hook every tool gets and current reads the machine's child.json
// as it is now (fleet.Gate's Confirm and Current). stop ends what the tools
// left running: the background commands and the browser. It is what a pause
// calls; the mux's own Close calls it too.
func buildChildMux(ctx context.Context, dir string, confirm confirmFunc, current func() (fleet.Child, error), log *slog.Logger) (mux *toolMux, stop func()) {
	data := childDataDir(dir)
	c := &commonFlags{backend: "echo", cfgDir: dir, dataDir: data}
	if err := c.applyLaunchConfig(); err != nil {
		// Without its config.json a child still serves the OS tools and the
		// browser; what that file would have added is left out.
		log.Warn("fleet child: config.json was not loaded; SQL, the wiki, iMessage and local MCP servers are not served", "err", err)
		c.lc = nil
	}
	mux = newToolMux()

	svc, _ := osys.Open(childExecConfig(dir, data, confirm, current)) // never fails
	mux.Add("os", childOS{svc: svc, policy: func() osys.Policy {
		var more []string
		if cur, err := current(); err == nil {
			more = cur.SecretPaths
		}
		return osys.Policy{Confirm: confirm, Roots: childRoots(dir, more)}
	}})

	// The browser starts when a call first needs it.
	browser := newLazyWeb(webConfig(dir, &pluginFlags{}, c.lc), web.Policy{Confirm: confirm}, filepath.Join(data, "screenshots"), nil, nil, log)
	mux.Add("web", browser)

	stop = func() {
		svc.Close()
		browser.Close()
	}
	mux.AddCleanup(stop)
	if c.lc == nil {
		return mux, stop
	}

	e := &pluginEnv{c: c, pf: &pluginFlags{}, confirm: confirm, log: log, wiki: &wikiLive{}}
	for _, id := range []string{"sql", "wiki", "imessage"} {
		sc := c.lc.Skills[id]
		if sc == nil || !sc.Enabled {
			continue // not configured on this host
		}
		if id == "wiki" {
			// The wiki's search needs an embedder and nothing else of a model: the
			// one config.json names under embed, else the built-in stand-in.
			router, err := finishModelRouter(model.NewEmptyRegistry(), c, log, nil)
			if err != nil {
				log.Warn("fleet child: the wiki is not served: no embedder", "err", err)
				continue
			}
			e.router = router
		}
		d, _ := plugins().Get(id)
		p, cleanup, err := relaunchFromConfig(ctx, d, e)
		if err != nil {
			if !errors.Is(err, errNotConfigured) {
				log.Warn("fleet child: a configured plugin did not start and is not served", "plugin", id, "err", err)
			}
			continue
		}
		mux.Add(id, p)
		mux.AddCleanup(cleanup)
	}

	// Local MCP servers start in the background; their tools join the list
	// when they are up, and the master reads the list each time it asks.
	mux.SetMCPDeps(mcpRuntimeDeps{Ctx: ctx, Confirm: confirm, Log: log})
	registerMCPServers(mux, c.lc, dir, data)
	return mux, stop
}

// childRunsCommand answers the no-exec ceiling's question for an approval
// action: does allowing it run a command on this machine? The os_exec family
// does. So does everything no plugin declares: a local MCP server's tool, or
// an action a later version adds, could run anything, and a ceiling that
// guesses "no" is not one.
func childRunsCommand(action string) bool {
	_, declared := plugins().ActionCategory(action)
	return !declared || strings.HasPrefix(action, "os_exec")
}

// childExecConfig is the os service of a child: its commands run through the
// sandbox backend its own child.json names, read for every command, and a
// refusal by that backend asks the master like any other gated action.
func childExecConfig(dir, data string, confirm confirmFunc, current func() (fleet.Child, error)) osys.Config {
	profile := func(context.Context) (execProfile, string, error) {
		cur, err := current()
		if err != nil {
			return execProfile{}, surfaceChild, err
		}
		p := execProfile{Backend: cur.Sandbox, Policy: sandbox.Policy{DenyRoots: tenantDenyRoots(dir, data), Env: envAllowlist(nil, nil)}}
		if p.Backend != "host" {
			p.Policy.Enforce = true // a backend the machine asked for and does not have runs nothing
			applyConfinedDefaults(&p)
		}
		return p, surfaceChild, nil
	}
	return osys.Config{
		Runner: func(ctx context.Context, command string, timeout time.Duration) (string, error) {
			p, surface, err := profile(ctx)
			if err != nil {
				return "", err
			}
			return profileRunner(p, surface, confirm)(ctx, command, timeout)
		},
		Starter: profileStarter(profile),
		ProcDir: filepath.Join(data, "proc"),
	}
}

// childOS is the OS plugin as a child serves it. The file tools' boundary is
// worked out for each call, so an account made after the child started, and
// a path the operator has just added to child.json, are covered at once.
type childOS struct {
	svc    *osys.Service
	policy func() osys.Policy
}

func (o childOS) Tools() []model.ToolSpec { return osys.NewDispatcher(nil, osys.Policy{}).Tools() }

func (o childOS) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(call.Arguments, &a) == nil && strings.TrimSpace(a.Path) != "" {
		// As given first: a share is not even looked at. Then as it resolves,
		// for a link on the disk that leads to one.
		refuse := notAFilePath(runtime.GOOS, a.Path, "")
		if !refuse {
			target, _ := osys.Roots{}.Resolve(a.Path, false)
			refuse = notAFilePath(runtime.GOOS, "", target)
		}
		if refuse {
			return "refused: " + a.Path + " names a device, a share or a volume, not a file on this machine's own disks. " +
				"The file tools of a fleet machine take ordinary local paths only: such a name can be a second way to a file the machine keeps secret. " +
				"This is a hard boundary, it is not approvable", true, nil
		}
	}
	return osys.NewDispatcher(o.svc, o.policy()).Dispatch(ctx, call)
}

// notAFilePath reports a Windows path that leaves the drive letters: a
// device (\\.\PhysicalDrive0 is the whole disk), a volume by its object name
// (\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy1 is last week's copy of
// every file, the password database among them) or a share (\\localhost\C$
// is this machine again). The secret list is matched by path and by the
// identity of the file, and none of the three has either in common with the
// file it reaches. Elsewhere the devices are directories and are denied as
// such (childDenyRoots).
func notAFilePath(goos, given, resolved string) bool {
	if goos != "windows" {
		return false
	}
	for _, p := range []string{given, resolved} {
		if p = strings.ReplaceAll(strings.TrimSpace(p), "/", `\`); strings.HasPrefix(p, `\\`) {
			return true
		}
	}
	return false
}

// childRoots is the boundary of a child's file tools. A child's master may
// read and write anywhere the child's account can, with three exceptions no
// approval opens:
//
//   - Secret, neither read nor written: the child's own directory (its key,
//     child.json, the audit log, config.json and the credentials beside it),
//     the machine's password databases and key stores, every account's
//     credential stores and browser profiles, and what the operator added
//     under secret_paths in child.json;
//   - Protect, never written: the running binary and the unit that starts
//     it;
//   - Deny: where the kernel shows devices and other processes' memory as
//     files.
//
// There are no write roots: a write anywhere else asks the master.
func childRoots(dir string, more []string) osys.Roots {
	secret := append([]string{dir}, machineSecrets(runtime.GOOS, os.Getenv)...)
	for _, home := range accountHomes(runtime.GOOS, os.Getenv) {
		roaming, local := "", ""
		if runtime.GOOS == "windows" {
			roaming, local = filepath.Join(home, "AppData", "Roaming"), filepath.Join(home, "AppData", "Local")
			secret = append(secret, filepath.Join(roaming, "Microsoft", "Protect"), filepath.Join(roaming, "Microsoft", "Crypto"),
				filepath.Join(roaming, "Microsoft", "Vault"), filepath.Join(local, "Microsoft", "Vault"))
		}
		secret = append(secret, credentialStoresIn(home, roaming, local)...)
		for _, rel := range []string{".password-store", ".local/share/keyrings", ".vault-token"} {
			secret = append(secret, filepath.Join(home, filepath.FromSlash(rel)))
		}
	}
	return osys.Roots{
		Deny:    childDenyRoots(runtime.GOOS),
		Secret:  append(secret, more...),
		Protect: dedupeNonEmpty(append([]string{exePath()}, childUnitFiles(dir)...)...),
	}
}

// childDenyRoots are the directories that are not files at all on goos.
func childDenyRoots(goos string) []string {
	switch goos {
	case "linux":
		return []string{"/proc", "/sys", "/dev"}
	case "darwin":
		return []string{"/dev"}
	}
	return nil // Windows: notAFilePath
}

// machineSecrets lists where goos itself keeps passwords and keys.
func machineSecrets(goos string, env func(string) string) []string {
	switch goos {
	case "windows":
		root, data, drive := env("SystemRoot"), env("ProgramData"), env("SystemDrive")
		if root == "" {
			root = `C:\Windows`
		}
		if data == "" {
			data = `C:\ProgramData`
		}
		if drive == "" {
			drive = `C:`
		}
		return []string{
			root + `\System32\config`,                                       // the SAM, SECURITY and SYSTEM hives, and the SYSTEM account's profile
			root + `\NTDS`,                                                  // a domain controller's directory
			root + `\System32\Microsoft\Protect`,                            // the machine's DPAPI keys
			root + `\repair`, root + `\Panther`, root + `\System32\Sysprep`, // copies of the hives; setup answer files
			data + `\Microsoft\Crypto`, data + `\Microsoft\Vault`,
			data + `\ssh\ssh_host_*_key`,
			drive + `\pagefile.sys`, drive + `\hiberfil.sys`, drive + `\swapfile.sys`, // memory, written out
		}
	case "darwin":
		return []string{"/etc/master.passwd", "/var/db/dslocal/nodes/Default/users", "/Library/Keychains",
			"/etc/krb5.keytab", "/etc/ssh/ssh_host_*_key"}
	}
	return []string{"/etc/shadow", "/etc/shadow-", "/etc/gshadow", "/etc/gshadow-", "/etc/security/opasswd",
		"/etc/krb5.keytab", "/etc/ssh/ssh_host_*_key", "/etc/ssl/private", "/etc/pki/tls/private", "/etc/letsencrypt",
		"/etc/NetworkManager/system-connections", "/etc/wpa_supplicant", "/var/lib/sss/db", "/var/lib/sss/secrets"}
}

// accountHomes lists the home folder of every account on the machine as it
// is now: a child that runs as root or SYSTEM reads all of them.
// ponytail: lists the homes on every file-tool call. Cache the list for a
// few seconds if a machine with thousands of accounts makes that slow.
func accountHomes(goos string, env func(string) string) []string {
	var homes, parents []string
	switch goos {
	case "windows":
		root, drive := env("SystemRoot"), env("SystemDrive")
		if root == "" {
			root = `C:\Windows`
		}
		if drive == "" {
			drive = `C:`
		}
		parents = []string{drive + `\Users`, root + `\ServiceProfiles`}
	case "darwin":
		homes, parents = []string{"/var/root"}, []string{"/Users"}
	default:
		homes, parents = []string{"/root"}, []string{"/home"}
	}
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, home) // wherever this account's is
	}
	for _, parent := range parents {
		entries, _ := os.ReadDir(parent)
		for _, e := range entries {
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				homes = append(homes, filepath.Join(parent, e.Name()))
			}
		}
	}
	slices.Sort(homes)
	return slices.Compact(homes)
}
