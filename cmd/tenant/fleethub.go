package main

// fleethub.go is the master's end of the fleet inside a running Tenant
// (TEN-531; docs/plans/gap6-fleet-plan.md, sections 3 and 5): the second
// listener, held by one process, and where a turn came from as a fleet call
// sees it.
//
// A Tenant becomes a master when `tenant fleet init` writes its fleet
// identity. serve and the terminal app each run startFleetHub, which looks
// at the config dir every few seconds, as the settings watcher does: once the
// identity is there it takes the fleet and listens, with no restart. One
// process holds the links (fleet.StartManager); the other is told which, and
// takes over when the holder exits. What the command line changes reaches
// the holder through the files it changes: fleet.json (the store watches
// it), the settings file (the settings watcher) and config.json (here).
//
// The rest of cmd/tenant sees the fleet through three functions: fleetManager,
// fleetStore and fleetOrigin.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/approval"
	"tenant/internal/fleet"
	"tenant/internal/lifecycle"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// defaultFleetListen is where the one fleet port binds unless config.json's
// fleet.listen says otherwise. Children dial in, so it is not a loopback
// address; nothing answers on it but the pinned link and the enroll endpoint.
const defaultFleetListen = "0.0.0.0:9777"

// fleetWatchInterval is how often a running Tenant looks for a change the
// command line made. Tests shorten it.
var fleetWatchInterval = settingsWatchInterval

// fleetListenGuard, when set, is asked before the fleet port is bound, and
// its error keeps the listener down. Nothing sets it in a shipped Tenant.
// The tests set it for their whole binary, helper processes included, to a
// check that refuses every address but a loopback one: the default is every
// interface, and a first listener there puts a firewall prompt on the screen
// of whoever is using the computer the tests run on.
var fleetListenGuard func(addr string) error

// wireFleetTools connects the fleet_* tools (fleettools.go) to this
// process's fleet: the listener it may hold, the fleet's files, where a turn
// came from and the machines' approval schemes. serve and the terminal app
// call it once, where the main agent's tools are built, so it is set before
// the first turn. A command-line process never calls it and has no fleet
// tools.
func wireFleetTools() {
	fleetDeps = &fleetToolDeps{manager: fleetManager, store: fleetStore, origin: fleetOrigin, schemes: defaultFleet, heldBy: fleetHeldElsewhere}
}

// fleetRelayRoster is the fleet's roster line for a relay's agent (discord,
// imessage): the machines, once the operator opened that relay for the
// fleet, and nothing before. A relay that may make no fleet call is not told
// which machines exist either.
func fleetRelayRoster(surface string) string {
	if h := theFleet.Load(); h == nil || !h.relayOpen(surface) {
		return ""
	}
	return fleetRoster()
}

// theFleet is this process's hub: nil in a command-line process, and in
// serve or the terminal app until startFleetHub runs.
var theFleet atomic.Pointer[fleetHub]

// fleetRelays are the relays' own brokers by exec surface (discord,
// imessage), registered where each relay is built. A relay turn's fleet call
// is checked by its broker as well as by the machine's scheme.
var fleetRelays sync.Map

// fleetRelayBroker registers a relay's broker for fleetOrigin.
func fleetRelayBroker(surface string, b *approvalBroker) { fleetRelays.Store(surface, b) }

// fleetLockPaths are the fleet's lock files, which nothing but the fleet may
// write (fileToolGuards).
func fleetLockPaths(cfgDir string) []string { return fleet.LockPaths(cfgDir) }

// fleetManager is the fleet this process holds: nil when another process
// holds it, or no fleet exists.
func fleetManager() *fleet.Manager {
	h := theFleet.Load()
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mgr
}

// fleetStore is the master's fleet.json: nil when no fleet was ever
// initialised in this config dir.
func fleetStore() *fleet.Store {
	h := theFleet.Load()
	if h == nil {
		return nil
	}
	if _, err := os.Lstat(fleet.MasterIdentityPath(h.cfgDir)); err != nil {
		return nil
	}
	return h.store
}

// fleetHeldElsewhere names the process that holds the fleet when it is not
// this one ("" otherwise): what /fleet and a fleet tool tell the operator
// instead of making a call.
func fleetHeldElsewhere() string {
	h := theFleet.Load()
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mgr != nil || !h.held {
		return ""
	}
	return (&fleet.HeldError{Holder: h.holder}).Error()
}

// fleetUpdater is this process's updates of fleet machines: nil in a
// command-line process.
func fleetUpdater() *fleetUpdates {
	if h := theFleet.Load(); h != nil {
		return h.updates
	}
	return nil
}

// fleetRollouter is this process's rollout of updates over many machines
// (fleetrollout.go): nil in a command-line process.
func fleetRollouter() *fleetRollout {
	if h := theFleet.Load(); h != nil {
		return h.rollout
	}
	return nil
}

type fleetMachinesKey struct{}

// withFleetMachines stamps an unattended turn with the fleet machines it may
// reach: a cron job's list of ids, or none for a turn that reaches no machine
// (an alert's investigation). A stamped turn is an unattended one whatever
// surface it runs on: nobody is asked, and nothing off the list is reached.
func withFleetMachines(ctx context.Context, ids []string) context.Context {
	return context.WithValue(ctx, fleetMachinesKey{}, ids)
}

// fleetOrigin is where ctx's turn came from, as a fleet call is checked: the
// local surfaces, a relay with its own rules and its fleet switch, or a cron
// job with its list of machines. It is read from the exec surface the turn
// was stamped with, so a turn nobody stamped is local and every other
// surface has to be one of the known ones: an origin this does not know
// reaches nothing. Before every call, reads included:
//
//	defaultFleet.For(fleetOrigin(ctx), machine).Reach()
func fleetOrigin(ctx context.Context) approval.Origin {
	surface := execSurfaceFrom(ctx)
	closed := approval.Origin{Name: surface}
	h := theFleet.Load()
	if h == nil {
		return closed
	}
	ids, unattended := ctx.Value(fleetMachinesKey{}).([]string)
	if unattended || surface == surfaceCronExec || surface == surfaceCronShell {
		// A job that was not opted into exec may read and never act; one that
		// was is checked by the cron-exec approver's own rules. Nobody is
		// there to ask either way.
		var allows func(action, detail string) bool
		if offsiteConfirmFrom(ctx) != nil {
			allows = cronExecAllows(h.cfgDir, h.dataDir)
		}
		return approval.CronOrigin(ids, allows)
	}
	switch surface {
	case surfaceLocal:
		return approval.LocalOrigin(h.local)
	case surfaceDiscord, surfaceIMessage:
		b, _ := fleetRelays.Load(surface)
		relay, _ := b.(*approvalBroker)
		if relay == nil {
			return closed
		}
		return approval.RelayOrigin(relay, h.relayOpen(surface))
	}
	return closed
}

// fleetHub is one process's view of the fleet in its config dir.
type fleetHub struct {
	cfgDir, dataDir string
	role            string // serve | tui: how another process names this one
	local           *approvalBroker
	tell            func(string) // the operator's status feed
	log             *slog.Logger
	store           *fleet.Store
	comp            *lifecycle.Comp
	act             *event.Activity // the fleet port's lifetime in the fleet log (fleet.Listening)
	updates         *fleetUpdates   // `tenant fleet update`, run here while this process holds the fleet
	rollout         *fleetRollout   // a staggered update of many machines, through updates

	mu      sync.Mutex
	mgr     *fleet.Manager
	run     fleetRun     // what mgr was started with
	held    bool         // another process holds the fleet
	holder  fleet.Holder // that process
	problem string       // why the listener is not up, when it should be
	said    string       // the state the operator was last told
	listen  string       // config.json's fleet.listen, or the default
	relays  map[string]bool
	cfgSeen [2]int64 // config.json's time and size when it was last read

	// The machines' memory (fleetmemory.go): open while this process holds
	// the fleet and the fleet key and recovery key are there.
	mem        *fleetMemory
	memProblem string // why mem is nil while mgr is not
	memInputs  string // the key file and recovery key it was last tried with
}

// fleetRun is what a manager was started with: a change of either means
// starting it again.
type fleetRun struct {
	identity int64 // the identity file's mtime
	listen   string
}

// startFleetHub makes this process a candidate to hold the fleet, and
// returns what stops it. It looks once before it returns, so a master that
// already has a fleet is listening when its start-up line is printed.
func startFleetHub(ctx context.Context, c *commonFlags, role string, local *approvalBroker, tell func(string), log *slog.Logger) (stop func()) {
	if tell == nil {
		tell = func(string) {}
	}
	h := &fleetHub{cfgDir: c.cfgDir, dataDir: c.dataDir, role: role, local: local, tell: tell, log: log,
		store: fleet.Open(c.cfgDir), listen: defaultFleetListen, relays: map[string]bool{}, updates: newFleetUpdates(c.dataDir)}
	h.rollout = newFleetRollout(h.updates)
	theFleet.Store(h)
	ctx, cancel := context.WithCancel(ctx)
	h.tick(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(fleetWatchInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.tick(ctx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		h.close()
		theFleet.CompareAndSwap(h, nil)
	}
}

func (h *fleetHub) relayOpen(surface string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.relays[surface]
}

// readConfig takes the fleet's part of config.json when the file changed:
// the bind address and the relays' fleet switches, which the command line
// (`tenant fleet init --listen`, `tenant fleet relay`) or the terminal may
// have set. While the file cannot be read the switches are off (a relay is
// not left open on the strength of a file nobody can read) and the listener
// stays where it is.
func (h *fleetHub) readConfig() {
	var seen [2]int64 // time and size: two saves inside one tick of the clock still differ
	if fi, err := os.Stat(launchConfigPath(h.cfgDir)); err == nil {
		seen = [2]int64{fi.ModTime().UnixNano(), fi.Size()}
	}
	h.mu.Lock()
	same, listen := seen == h.cfgSeen && seen != [2]int64{}, h.listen
	h.mu.Unlock()
	if same {
		return
	}
	relays := map[string]bool{}
	if lc, err := loadLaunchConfig(h.cfgDir); err == nil {
		listen = defaultFleetListen
		if lc.Fleet.Listen != "" {
			listen = lc.Fleet.Listen
		}
		relays[surfaceDiscord], relays[surfaceIMessage] = lc.Relay.AllowFleet, lc.IMessage.AllowFleet
	}
	h.mu.Lock()
	h.cfgSeen, h.listen, h.relays = seen, listen, relays
	h.mu.Unlock()
}

// tick brings the listener in line with the config dir: up when a fleet
// identity is there and nobody else holds the fleet, on the address
// config.json names, and down when the identity is gone.
func (h *fleetHub) tick(ctx context.Context) {
	// The identity is looked for first and config.json read second. init
	// writes them the other way round, so an identity seen here means the
	// bind address init set is on disk: the listener never starts on the
	// address init was about to replace.
	defer h.memoryTick()
	path := fleet.MasterIdentityPath(h.cfgDir)
	fi, err := os.Lstat(path)
	h.readConfig()
	h.mu.Lock()
	mgr, was, listen := h.mgr, h.run, h.listen
	h.mu.Unlock()
	if err != nil { // no fleet here (or its identity was removed)
		if mgr != nil {
			h.close()
		}
		h.state(false, fleet.Holder{}, "", "")
		return
	}
	want := fleetRun{identity: fi.ModTime().UnixNano(), listen: listen}
	if mgr != nil && was == want {
		return
	}
	if mgr != nil { // the bind address or the identity changed under a running listener
		h.close()
	}
	id, err := fleet.LoadIdentity(path)
	if err == nil && fleetListenGuard != nil {
		err = fleetListenGuard(listen)
	}
	if err != nil {
		h.failed(ctx, err)
		return
	}
	mgr, err = fleet.StartManager(fleet.ManagerConfig{Store: h.store, Identity: id, Addr: listen, Role: h.role,
		Refused: h.refused, Alert: h.alert, Connected: h.connected})
	var held *fleet.HeldError
	switch {
	case errors.As(err, &held):
		who := strings.TrimPrefix(held.Error(), "fleet: ")
		if h.state(true, held.Holder, "", who+". Its machines are called from there; /fleet here lists them.") {
			fleet.HeldElsewhere.Log(ctx, "holder", who)
		}
		return
	case err != nil:
		h.failed(ctx, err)
		return
	}
	h.mu.Lock()
	h.mgr, h.run = mgr, want
	h.mu.Unlock()
	h.comp = lifecycle.Track(ctx, "fleet listener", lifecycle.Server, "system", lifecycle.Running, nil)
	_, h.act = event.Start(ctx, fleet.Listening, "addr", mgr.Addr().String(), "role", h.role)
	say := "listening on " + mgr.Addr().String()
	if h.role != "serve" {
		say += ". Machines are connected only while this app is open: a master with a fleet should run `tenant serve` as its service (tenant service install)."
	}
	h.state(false, fleet.Holder{}, "", say)
}

// failed records why the listener is not up. It is tried again at each tick.
func (h *fleetHub) failed(ctx context.Context, err error) {
	h.mu.Lock()
	fresh, listen := h.problem != err.Error(), h.listen
	h.mu.Unlock()
	if fresh {
		sysUnavailable.Log(ctx, "component", "the fleet listener", "effect", "no fleet machine can connect", "err", err)
		fleet.ListenerNotStarted.Log(ctx, "addr", listen, "err", err)
	}
	h.state(false, fleet.Holder{}, err.Error(), "listener not started: "+err.Error())
}

// state records where the fleet stands and tells the operator when that is
// not what they were last told; it reports whether it is news.
func (h *fleetHub) state(held bool, holder fleet.Holder, problem, say string) (fresh bool) {
	h.mu.Lock()
	h.held, h.holder, h.problem = held, holder, problem
	fresh = say != h.said
	h.said = say
	h.mu.Unlock()
	if fresh && say != "" {
		h.tell("fleet: " + say)
	}
	return fresh
}

// close ends this process's hold on the fleet, if it has one.
func (h *fleetHub) close() {
	h.mu.Lock()
	mgr, mem := h.mgr, h.mem
	h.mgr, h.run = nil, fleetRun{}
	h.mem, h.memProblem, h.memInputs = nil, "", ""
	h.mu.Unlock()
	if mem != nil {
		mem.close() // before the links: its comparisons end, and every shadow is written out
	}
	if mgr == nil {
		return
	}
	_ = mgr.Close()
	if h.comp != nil {
		h.comp.Set(lifecycle.Stopped, nil)
	}
	if h.act != nil {
		h.act.End(nil)
		h.act = nil
	}
}

// memoryTick opens the machines' memory once this process holds the fleet,
// and tries again when the fleet key or the recovery key changed (init made
// them since), or when another process held the memory. While it is open it
// carries out deletions another process asked for.
func (h *fleetHub) memoryTick() {
	h.mu.Lock()
	mgr, mem, tried := h.mgr, h.mem, h.memInputs
	h.mu.Unlock()
	if mgr == nil {
		return
	}
	if mem != nil {
		mem.sweep()
		return
	}
	inputs := "none"
	if fi, err := os.Stat(fleet.KeyPath(h.cfgDir)); err == nil {
		inputs = fmt.Sprint(fi.ModTime().UnixNano(), fi.Size())
	}
	rec, _, err := h.store.Recovery()
	inputs += " " + rec + fmt.Sprint(err != nil)
	if inputs == tried {
		return
	}
	mem, problem := openFleetMemory(h.cfgDir, h.dataDir, h.store, mgr, h.log, h.tell)
	if problem == errMemoryHeld.Error() {
		inputs = "" // tried again at the next tick
	}
	h.mu.Lock()
	if h.mgr != mgr { // given up meanwhile
		h.mu.Unlock()
		if mem != nil {
			mem.close()
		}
		return
	}
	said := h.memProblem
	h.mem, h.memProblem, h.memInputs = mem, problem, inputs
	h.mu.Unlock()
	if mem == nil {
		if problem != said {
			h.tell("fleet: the machines' memory is unavailable: " + problem)
			fleet.MemoryUnavailable.Log(context.Background(), "why", problem)
		}
		return
	}
	// Machines that connected before the memory was open are compared now.
	all, err := mgr.Machines()
	fleet.MemoryReady.Log(context.Background(), "machines", len(all))
	if err == nil {
		for _, st := range all {
			if st.Online {
				mem.kick(st.ID)
			}
		}
	}
}

// connected is told by the manager of each machine whose link is up.
func (h *fleetHub) connected(id string) {
	h.mu.Lock()
	mem := h.mem
	h.mu.Unlock()
	if mem != nil {
		mem.kick(id)
	}
}

// refused files the audit record of an enrollment attempt the endpoint
// turned away. A replayed code is the alert's to record.
func (h *fleetHub) refused(r fleet.Refusal) {
	if r.Reason == fleet.RefusedReplay {
		return
	}
	_ = catalog.FleetEnrollRefused.Audit(context.Background(), "reason", string(r.Reason), "remote", r.Source,
		"name", r.Name, "machine", r.Machine, "certificate", r.Fingerprint)
	fleet.EnrollRefused.Log(context.Background(), "reason", string(r.Reason), "remote", r.Source, "name", r.Name, "machine", r.Machine)
}

// alert is raised when a spent enrollment code is presented again: someone
// other than the machine it enrolled holds the code, or held it first. The
// operator is told where they look (the feed; serve's log) and the Security
// log keeps it as an error, which the alert rules can act on.
func (h *fleetHub) alert(r fleet.Refusal) {
	_ = catalog.FleetCodeReplayed.Audit(context.Background(), "name", r.Name, "machine", r.Machine, "remote", r.Source, "certificate", r.Fingerprint)
	fleet.CodeReplayed.Log(context.Background(), "name", r.Name, "machine", r.Machine, "remote", r.Source)
	h.tell(fmt.Sprintf("fleet: ALERT: the enrollment code for %s was presented a second time, from %s, and refused. "+
		"Someone other than the machine it enrolled (%s) has that code, or had it first: compare the six digits on the machine's own screen before you confirm it, or `tenant fleet revoke %s` and enroll it again.",
		fleetQuote(r.Name), fleetQuote(r.Source), r.Machine, r.Machine))
}

// fleetIdentity is the master's fleet identity in cfgDir. A Tenant that was
// never made a master has none.
func fleetIdentity(cfgDir string) (fleet.Identity, error) {
	id, err := fleet.LoadIdentity(fleet.MasterIdentityPath(cfgDir))
	if errors.Is(err, fs.ErrNotExist) {
		return fleet.Identity{}, errors.New("this Tenant has no fleet yet: run `tenant fleet init`")
	}
	return id, err
}
