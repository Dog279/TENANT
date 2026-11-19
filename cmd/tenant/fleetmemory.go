package main

// fleetmemory.go is the master's memory of its machines, wired to the link
// (TEN-532; docs/plans/gap6-fleet-plan.md, section 6). The library is
// internal/fleet/shadow: a machine's shadow lives in RAM, its snapshot on disk
// encrypted to the fleet key and the recovery key; its child keeps the last
// three generations, which it cannot read. Here the process that holds the
// fleet opens the fleet key and the shadows, and on every reconnect and every
// five minutes compares each machine's copy with its own and acts on "who
// wins" (shadow.Decide).
//
// Concurrency. A machine's comparison runs on a goroutine of its own, at most
// one per machine: a reconnect or the timer that asks for one while it runs
// makes it go round once more, and nothing waits for it. The shadow store's
// calls each take that machine's lock for a local disk or RAM operation and
// give it back; no lock of the store or of this file is held across a call
// on the link, and a snapshot read for sending is a copy of the bytes. A
// shadow a tool holds is never waited for: Adopt answers ErrInUse at once and
// the comparison is left for the next pass. The operator's view (State) is a
// map read under a mutex no slow work takes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"tenant/internal/datalock"
	"tenant/internal/fleet"
	"tenant/internal/fleet/shadow"
	"tenant/internal/fsperm"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
	"tenant/internal/secrets"
)

// A machine's memory as the operator sees it: the values the dashboard's
// Fleet page shows (dashboard.FleetMemory*, TEN-533). memoryUnknown is also
// "unavailable"; the detail says which, and why.
const (
	memoryUnknown  = ""
	memoryInStep   = "in_step"
	memoryBehind   = "behind"
	memoryNeedsKey = "needs_recovery_key"
)

var (
	// fleetMemoryEvery is how often every machine in RAM gets a new
	// generation if it changed, and every machine online is compared. Tests
	// shorten it.
	fleetMemoryEvery = 5 * time.Minute
	// fleetShadowOptions tune the shadow store; the zero value is what ships.
	fleetShadowOptions shadow.Options
	// fleetEmbedder is what a shadow rebuilt from a child's copy, which comes
	// without its vectors, is re-embedded with: set by serve and the terminal
	// app from their router. Until it is set a rebuilt shadow is searched by
	// its words alone, and the re-embed is tried again at its next comparison.
	fleetEmbedder atomic.Pointer[model.Embedder]
)

// setFleetEmbedder gives the fleet's memory the process's embedder.
func setFleetEmbedder(emb model.Embedder) {
	if emb != nil {
		fleetEmbedder.Store(&emb)
	}
}

// fleetShadowDir is where the shadows' snapshots are kept: <data>/fleet.
func fleetShadowDir(dataDir string) string { return filepath.Join(dataDir, "fleet") }

// fleetMemoryStatesPath is the record of every machine's memory state the
// holder keeps for the other processes (tenant fleet show). Nothing secret.
func fleetMemoryStatesPath(dataDir string) string {
	return filepath.Join(fleetShadowDir(dataDir), "states.json")
}

// memoryRemoveMark, in a machine's shadow directory, asks the process that
// holds the fleet to delete that machine's memory: a revoke with delete made
// by another process.
const memoryRemoveMark = "remove"

// memoryState is one machine's memory state.
type memoryState struct {
	State  string    `json:"state"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// fleetMemory is the master's memory of its machines, open in the process
// that holds the fleet.
type fleetMemory struct {
	dataDir  string
	mgr      *fleet.Manager
	store    *fleet.Store
	key      *shadow.Key
	recovery string // the recovery key's public half
	shadows  *shadow.Store
	log      *slog.Logger
	tell     func(string)

	// The forgets every machine's memory has had or is owed
	// (fleetforget.go). Held across the local work of applying one, never
	// across a call on the link.
	fgMu    sync.Mutex
	forgets []*fleetForget

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	states   map[string]memoryState
	syncing  map[string]bool   // a comparison runs
	again    map[string]bool   // and was asked for again meanwhile
	childGen map[string]uint64 // the newest generation under this key each machine was last seen to hold
	reembed  map[string]bool   // rebuilt, and not re-embedded yet

	fileMu sync.Mutex // one write of the states file at a time
}

// errMemoryHeld is the shadows' lock held by another process.
var errMemoryHeld = errors.New("another Tenant process holds the fleet's memory")

// openFleetMemory opens the fleet key and the shadows for the process that
// holds the fleet. When it cannot, problem says why in a sentence, every
// machine's memory is unavailable for that reason, and nothing is touched.
func openFleetMemory(cfgDir, dataDir string, store *fleet.Store, mgr *fleet.Manager, log *slog.Logger, tell func(string)) (*fleetMemory, string) {
	recipient, _, err := store.Recovery()
	switch {
	case err != nil:
		return nil, "fleet.json cannot be read: " + err.Error()
	case recipient == "":
		return nil, "the fleet has no recovery key yet: run `tenant fleet init` at a terminal"
	}
	key, err := shadow.OpenKey(fleet.KeyPath(cfgDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, "the fleet has no fleet key yet: run `tenant fleet init`"
	case err != nil:
		return nil, "the fleet key cannot be opened: " + err.Error()
	}
	shadows, err := shadow.OpenStore(fleetShadowDir(dataDir), key, recipient, withShadowErrors(fleetShadowOptions, log))
	if err != nil {
		_ = key.Close()
		if errors.Is(err, datalock.ErrBusy) {
			return nil, errMemoryHeld.Error()
		}
		return nil, "the machines' memory cannot be opened: " + err.Error()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &fleetMemory{dataDir: dataDir, mgr: mgr, store: store, key: key, recovery: recipient, shadows: shadows, log: log, tell: tell, ctx: ctx, cancel: cancel,
		states: map[string]memoryState{}, syncing: map[string]bool{}, again: map[string]bool{}, childGen: map[string]uint64{}, reembed: map[string]bool{}}
	if locked, why := key.Locked(); !locked {
		log.Warn("fleet: the fleet key is held in memory that is not locked against the swap file", "why", why)
	}
	m.loadForgets()
	m.sweep()
	m.wg.Add(1)
	go m.run()
	return m, ""
}

func withShadowErrors(opt shadow.Options, log *slog.Logger) shadow.Options {
	if opt.OnError == nil {
		opt.OnError = func(host string, err error) {
			log.Warn("fleet: a machine's memory could not be written out", "machine", host, "err", err)
		}
	}
	return opt
}

// close stops the comparisons, writes every shadow out and lets go of the key.
func (m *fleetMemory) close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()
	if err := m.shadows.Close(); err != nil {
		m.log.Warn("fleet: the machines' memory did not close cleanly", "err", err)
	}
	_ = m.key.Close()
}

// run is the five-minute pass: a new generation of every shadow in RAM that
// changed, whether or not its machine is reachable, and a comparison with
// every machine online.
func (m *fleetMemory) run() {
	defer m.wg.Done()
	t := time.NewTicker(fleetMemoryEvery)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		m.pass()
	}
}

func (m *fleetMemory) pass() {
	for _, id := range m.shadows.Loaded() {
		c, made, err := m.shadows.Mint(m.ctx, id)
		if err != nil {
			m.log.Warn("fleet: no new generation of a machine's memory", "machine", id, "err", err)
			continue
		}
		if made {
			m.minted(id, c)
		}
	}
	machines, err := m.mgr.Machines()
	if err != nil {
		return
	}
	for _, st := range machines {
		if st.Online && st.State == fleet.StateConfirmed {
			m.kick(st.ID)
		}
	}
}

// minted records a generation made while the machine may not have it yet.
func (m *fleetMemory) minted(id string, c shadow.Copy) {
	m.mu.Lock()
	have, now := m.childGen[id], m.states[id].State
	m.mu.Unlock()
	if have < c.Gen && now != memoryNeedsKey { // that stays until the recovery identity is brought
		m.set(id, memoryBehind, fmt.Sprintf("the master made generation %d; the machine has %s", c.Gen, genText(have)))
	}
}

func genText(gen uint64) string {
	if gen == 0 {
		return "none yet"
	}
	return fmt.Sprintf("generation %d", gen)
}

// kick compares a machine's memory with its child's, now, on a goroutine of
// its own; if that is running already it goes round once more.
func (m *fleetMemory) kick(id string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if m.syncing[id] {
		m.again[id] = true
		m.mu.Unlock()
		return
	}
	m.syncing[id] = true
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		for {
			m.sync(id)
			m.mu.Lock()
			if !m.again[id] || m.closed {
				delete(m.syncing, id)
				delete(m.again, id)
				m.mu.Unlock()
				return
			}
			delete(m.again, id)
			m.mu.Unlock()
		}
	}()
}

// syncRounds bounds one comparison: each round acts once and states again.
const syncRounds = 12

// sync is "who wins" for one machine, every row of the spec's table, until
// there is nothing left to do (shadow.Decide).
func (m *fleetMemory) sync(id string) {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Minute)
	defer cancel()
	refused := map[shadow.Copy]bool{} // the child's copies that did not verify here
	for range syncRounds {
		if ctx.Err() != nil {
			return
		}
		st, err := m.shadows.State(id)
		if err != nil {
			m.set(id, memoryUnknown, "the master's memory of this machine cannot be read: "+err.Error())
			return
		}
		copies, err := m.mgr.MemoryList(ctx, id)
		if err != nil {
			m.unreachable(id, st, err)
			return
		}
		m.sawChild(id, st.KeyID, copies)
		for i := range copies {
			if refused[name(copies[i])] {
				copies[i].OK = false
			}
		}
		d := shadow.Decide(st, copies)
		switch d.Action {
		case shadow.Nothing:
			m.inStep(id, st)
			m.reembedIfOwed(ctx, id)
			m.purgeIfOwed(ctx, id) // after a forget: the machine holds the generation that has it
			return
		case shadow.Lost:
			m.set(id, memoryUnknown, "the master's memory of this machine is damaged and no copy of it is left on either side; nothing was changed")
			return
		case shadow.NeedsRecoveryKey:
			m.needsKey(id)
			return
		case shadow.Push:
			if !m.push(ctx, id, st, refused) {
				return
			}
		case shadow.Pull, shadow.Rebuild:
			switch m.take(ctx, id, d) {
			case takeRefused:
				refused[name(d.Copy)] = true
			case takeStop:
				return
			}
		}
	}
	m.set(id, memoryUnknown, fmt.Sprintf("the comparison with the machine did not settle in %d rounds; it is tried again in five minutes", syncRounds))
}

// name is a copy without what its holder says of its state, as a map key.
func name(c shadow.Copy) shadow.Copy {
	c.OK = true
	return c
}

// sawChild notes the newest intact generation the child holds under key.
func (m *fleetMemory) sawChild(id, key string, copies []shadow.Copy) {
	var newest uint64
	for _, c := range copies {
		if c.KeyID == key && c.OK {
			newest = max(newest, c.Gen)
		}
	}
	m.mu.Lock()
	m.childGen[id] = newest
	m.mu.Unlock()
}

// unreachable is a machine that could not be asked for its copies.
func (m *fleetMemory) unreachable(id string, st shadow.Master, err error) {
	switch {
	case errors.Is(err, fleet.ErrNoMemory):
		m.set(id, memoryUnknown, "this machine's Tenant keeps no copy of its memory yet (it is older than the master's); update it")
		return
	case errors.Is(err, fleet.ErrOffline):
		m.mu.Lock()
		have := m.childGen[id]
		m.mu.Unlock()
		if st.Gen > have {
			m.set(id, memoryBehind, fmt.Sprintf("the master has generation %d; the machine, offline, had %s when last seen", st.Gen, genText(have)))
		}
		return
	}
	m.set(id, memoryBehind, "the machine could not be asked for its copies: "+err.Error())
}

func (m *fleetMemory) inStep(id string, st shadow.Master) {
	m.mu.Lock()
	m.childGen[id] = st.Gen
	m.mu.Unlock()
	if st.Gen == 0 {
		m.set(id, memoryInStep, "no memory of this machine is kept yet")
		return
	}
	m.set(id, memoryInStep, fmt.Sprintf("generation %d on both sides", st.Gen))
}

func (m *fleetMemory) needsKey(id string) {
	const detail = "its memory, on the machine or in the master's own snapshot, was made under another fleet key; nothing is sent, fetched or overwritten until the recovery identity is brought"
	m.mu.Lock()
	was := m.states[id].State
	m.mu.Unlock()
	m.set(id, memoryNeedsKey, detail)
	if was != memoryNeedsKey {
		m.log.Warn("fleet: a machine's memory needs the recovery key", "machine", id)
		m.tell(fmt.Sprintf("fleet: the memory of machine %s needs the recovery key: %s", id, detail))
	}
}

// maxSkip is how far past its own newest generation the master numbers its
// next one, to get past a copy on the machine that is not one of its making.
const maxSkip = 1 << 20

// push sends the master's newest generation. It reports whether to go round
// again.
func (m *fleetMemory) push(ctx context.Context, id string, st shadow.Master, refused map[shadow.Copy]bool) bool {
	c, blob, err := m.shadows.Newest(id)
	if err != nil {
		m.set(id, memoryBehind, "the master's newest generation cannot be read: "+err.Error())
		return false
	}
	err = m.mgr.MemoryPut(ctx, id, c, blob)
	if errors.Is(err, shadow.ErrStale) {
		// The child takes a file of its own for a newer copy, which the master
		// fetched and found is not one of its making (refused). The master
		// numbers its next generation past it: past that one only, and not
		// far past its own, so a machine cannot use up the numbers.
		var past uint64
		for have := range refused {
			if have.KeyID == st.KeyID {
				past = max(past, have.Gen)
			}
		}
		switch {
		case past == 0:
			err = fmt.Errorf("%w, and none of the copies it listed was found not to be the master's", err)
		case past > st.Gen+maxSkip:
			err = fmt.Errorf("%w: it holds a copy it calls generation %d, which is not the master's, far past the master's %d; delete the machine's copies (%s)", err, past, st.Gen, "the memory folder in its child directory")
		default:
			if _, err = m.shadows.Skip(ctx, id, past); err == nil {
				return true
			}
		}
	}
	if err != nil {
		m.set(id, memoryBehind, fmt.Sprintf("generation %d is not on the machine yet: %v", c.Gen, err))
		return false
	}
	return true
}

type takeResult int

const (
	takeAgain   takeResult = iota // taken: go round again
	takeRefused                   // the copy did not verify: pass it over
	takeStop                      // nothing more this time
)

// take makes the child's copy, or the master's own newest generation, the
// shadow: the child wins, and a damaged shadow is rebuilt.
func (m *fleetMemory) take(ctx context.Context, id string, d shadow.Decision) takeResult {
	c, blob, err := d.Copy, []byte(nil), error(nil)
	if d.Own {
		c, blob, err = m.shadows.Newest(id)
	} else {
		blob, err = m.mgr.MemoryGet(ctx, id, c)
	}
	switch {
	case err == nil:
	case !d.Own && !errors.Is(err, fleet.ErrOffline) && !errors.Is(err, fleet.ErrOutcomeUnknown) && ctx.Err() == nil:
		// What the machine sent is not the copy it listed, or it no longer
		// has it: its next older copy is tried.
		m.log.Warn("fleet: a machine's copy of its memory did not come whole", "machine", id, "generation", c.Gen, "err", err)
		return takeRefused
	default:
		m.set(id, memoryBehind, fmt.Sprintf("the machine's generation %d could not be fetched: %v", c.Gen, err))
		return takeStop
	}
	got, err := m.shadows.Adopt(ctx, id, c, blob, nil)
	switch {
	case err == nil:
	case errors.Is(err, shadow.ErrInUse):
		m.set(id, memoryBehind, fmt.Sprintf("the machine's generation %d waits: the machine's memory is in use; it is tried again in five minutes", c.Gen))
		return takeStop
	case !d.Own && (errors.Is(err, shadow.ErrDamaged) || errors.Is(err, shadow.ErrForeignKey)):
		m.log.Warn("fleet: a machine's copy of its memory did not verify", "machine", id, "generation", c.Gen, "err", err)
		return takeRefused
	default:
		m.set(id, memoryUnknown, fmt.Sprintf("the machine's memory could not be rebuilt from generation %d: %v", c.Gen, err))
		return takeStop
	}
	what := "rebuilt from"
	if d.Action == shadow.Pull {
		what = "replaced by the machine's"
	}
	m.log.Info("fleet: a machine's memory was "+what+" a copy", "machine", id, "generation", got.Gen, "aside", got.Aside)
	if d.Diverged {
		m.tell(fmt.Sprintf("fleet: machine %s held generation %d with other content than this master's: the machine's copy stands, and this master's was kept aside for thirty days in %s", id, c.Gen, got.Aside))
		m.log.Warn("fleet: a machine's memory and the master's diverged; the machine's stands", "machine", id, "generation", c.Gen, "aside", got.Aside)
	}
	// A copy as old as a forget, or older, may hold what it removed.
	m.reapplyForgets(ctx, id, got.Gen)
	if got.Reembed {
		m.mu.Lock()
		m.reembed[id] = true
		m.mu.Unlock()
		m.reembedIfOwed(ctx, id)
	}
	return takeAgain
}

// reembedIfOwed gives a rebuilt shadow its vectors back with the existing
// re-embed functions, then writes it out.
func (m *fleetMemory) reembedIfOwed(ctx context.Context, id string) {
	m.mu.Lock()
	owed := m.reembed[id]
	m.mu.Unlock()
	emb := fleetEmbedder.Load()
	if !owed || emb == nil {
		return
	}
	if err := reembedShadow(ctx, m.shadows, id, *emb); err != nil {
		m.log.Warn("fleet: a rebuilt memory was not re-embedded; it is tried again at the next comparison", "machine", id, "err", err)
		return
	}
	m.mu.Lock()
	delete(m.reembed, id)
	m.mu.Unlock()
}

// reembedShadow is `tenant memory reembed` on one machine's shadow: a vector
// for every episode and fact at the embedder's dimension. The stores are
// released before Flush: a Store call for this machine waits for them.
func reembedShadow(ctx context.Context, shadows *shadow.Store, id string, emb model.Embedder) error {
	probe, err := emb.Embed(ctx, []string{"dim probe"})
	if err != nil || len(probe) != 1 {
		return fmt.Errorf("probe the embedder: %w", err)
	}
	p, err := shadows.Acquire(ctx, id)
	if err != nil {
		return err
	}
	err = func() error {
		defer p.Release()
		eps, err := p.Episodic.List(ctx, episodic.ListFilter{IncludeTombstoned: true})
		if err != nil {
			return err
		}
		if _, _, err := reembedEpisodes(ctx, emb, p.Episodic, eps, len(probe[0]), string(model.RoleEmbedder)); err != nil {
			return err
		}
		facts, err := p.Semantic.List(ctx, semantic.ListFilter{})
		if err != nil {
			return err
		}
		_, _, err = reembedFacts(ctx, emb, p.Semantic, facts, len(probe[0]), string(model.RoleEmbedder))
		return err
	}()
	if err != nil {
		return err
	}
	return shadows.Flush(ctx, id)
}

// set records a machine's state, and writes the record for the other
// processes when it changed.
func (m *fleetMemory) set(id, state, detail string) {
	detail = printableDetail(detail)
	m.mu.Lock()
	old := m.states[id]
	changed := old.State != state || old.Detail != detail
	if changed {
		m.states[id] = memoryState{State: state, Detail: detail, At: time.Now().UTC().Truncate(time.Second)}
	}
	all := make(map[string]memoryState, len(m.states))
	for k, v := range m.states {
		all[k] = v
	}
	m.mu.Unlock()
	if changed {
		m.writeStates(all)
	}
}

func (m *fleetMemory) writeStates(all map[string]memoryState) {
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	b, err := json.MarshalIndent(all, "", "  ")
	if err == nil {
		err = fsperm.Replace(fleetMemoryStatesPath(m.dataDir), append(b, '\n'))
	}
	if err != nil {
		m.log.Debug("fleet: the memory states were not recorded for other processes", "err", err)
	}
}

// state is one machine's state as last found.
func (m *fleetMemory) state(id string) (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[id]
	if !ok {
		return memoryUnknown, "not compared yet: the machine has not connected since this master took the fleet"
	}
	return st.State, st.Detail
}

// sweep carries out what other processes left for the process that holds
// the fleet, at every tick of the hub: the memory of machines revoked with
// delete (removeFleetMemory left the mark), the inventory of machines just
// confirmed (markInventory), and forgets (recordFleetForget). A shadow in
// use is left for the next sweep.
func (m *fleetMemory) sweep() {
	ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
	defer cancel()
	entries, _ := os.ReadDir(fleetShadowDir(m.dataDir))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(fleetShadowDir(m.dataDir), e.Name(), memoryRemoveMark)); err == nil {
			if err := m.shadows.Remove(e.Name()); err == nil {
				m.forget(e.Name())
				m.log.Info("fleet: a revoked machine's memory was deleted", "machine", e.Name())
			}
			continue
		}
		if _, err := os.Lstat(filepath.Join(fleetShadowDir(m.dataDir), e.Name(), inventoryMark)); err == nil {
			m.takeInventory(ctx, e.Name())
		}
	}
	m.takeForgetRequests()
	m.applyForgets(ctx)
}

// forget drops what is known of a machine whose memory is gone.
func (m *fleetMemory) forget(id string) {
	m.mu.Lock()
	delete(m.states, id)
	delete(m.childGen, id)
	delete(m.reembed, id)
	m.mu.Unlock()
}

// --- for the rest of cmd/tenant ---

// errNoFleetMemory is fleetShadow in a process whose machines' memory is not
// open: it does not hold the fleet, or the keys are missing.
var errNoFleetMemory = errors.New("fleet: the machines' memory is not open in this process")

// fleetShadow is one machine's memory, its two stores, for a caller in the
// process that holds the fleet: what is written into a machine's memory and
// read back from it goes through here. Release the Pair when done, and do
// not call the shadow store for this machine while holding it inside a
// transaction or an open result of its stores (one connection: it would wait
// on itself). A shadow that does not load (its snapshot is damaged, or was
// made under another fleet key) asks for a comparison with the machine,
// which rebuilds it from the machine's copy or marks it as needing the
// recovery key; the error says which.
func fleetShadow(ctx context.Context, id string) (*shadow.Pair, error) {
	mem, err := fleetMem()
	if err != nil {
		return nil, err
	}
	p, err := mem.shadows.Acquire(ctx, id)
	if errors.Is(err, shadow.ErrDamaged) || errors.Is(err, shadow.ErrForeignKey) {
		mem.kick(id)
	}
	return p, err
}

// fleetMemoryState is a machine's memory as the operator sees it: in step,
// behind, needs the recovery key, or unknown/unavailable with the reason in
// detail. It loads and decrypts nothing and is safe from any goroutine: the
// holder answers from what its last comparison found, another process from
// the record the holder keeps (tenant fleet show).
func fleetMemoryState(id string) (state, detail string) { return memoryStateIn("", id) }

// memoryStateIn is fleetMemoryState for a process that may hold no fleet at
// all (the command line), which reads the holder's record under dataDir.
func memoryStateIn(dataDir, id string) (state, detail string) {
	h := theFleet.Load()
	if h == nil {
		if dataDir == "" {
			return memoryUnknown, "known to the process that holds the fleet"
		}
		return recordedMemoryState(dataDir, id)
	}
	h.mu.Lock()
	mem, problem, holds, data := h.mem, h.memProblem, h.mgr != nil, h.dataDir
	h.mu.Unlock()
	switch {
	case mem != nil:
		return mem.state(id)
	case holds:
		return memoryUnknown, "unavailable: " + problem
	}
	return recordedMemoryState(data, id)
}

// fleetMemoryStates is fleetMemoryState with the shape the dashboard's Fleet
// page asks for (fleetMemoryOf, TEN-533).
type fleetMemoryStates struct{}

func (fleetMemoryStates) State(id string) (string, string) { return fleetMemoryState(id) }

// recordedMemoryState is a machine's memory state as the process holding the
// fleet last recorded it, for a process that does not hold it.
func recordedMemoryState(dataDir, id string) (string, string) {
	b, err := os.ReadFile(fleetMemoryStatesPath(dataDir))
	if err != nil || len(b) > 4<<20 {
		return memoryUnknown, "known to the process that holds the fleet"
	}
	var all map[string]memoryState
	if json.Unmarshal(b, &all) != nil {
		return memoryUnknown, "known to the process that holds the fleet"
	}
	st, ok := all[id]
	if !ok {
		return memoryUnknown, "not compared yet"
	}
	switch st.State {
	case memoryInStep, memoryBehind, memoryNeedsKey:
	default:
		st.State = memoryUnknown
	}
	return st.State, printableDetail(st.Detail) + " (as the process that holds the fleet recorded it at " + st.At.Local().Format(time.DateTime) + ")"
}

// printableDetail keeps a state's detail to one line of printable text, at
// most 1 KiB: it can quote what a machine said, and it is shown on a screen.
func printableDetail(s string) string {
	s = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if len(s) > 1024 {
		s = strings.ToValidUTF8(s[:1024], "") + "..."
	}
	return s
}

// memoryWords is a state as a screen says it.
func memoryWords(state string) string {
	switch state {
	case memoryInStep:
		return "in step"
	case memoryBehind:
		return "behind"
	case memoryNeedsKey:
		return "needs recovery key"
	}
	return "unknown"
}

// memoryLine is a machine's memory for /fleet show and tenant fleet show.
func memoryLine(dataDir, id string) string {
	state, detail := memoryStateIn(dataDir, id)
	line := "  memory      " + memoryWords(state)
	if detail != "" {
		line += ": " + detail
	}
	return line
}

// memoryGoneWords says where the deletion of a machine's memory stands.
func memoryGoneWords(dataDir, id string) string {
	if _, err := os.Lstat(filepath.Join(fleetShadowDir(dataDir), id)); errors.Is(err, fs.ErrNotExist) {
		return "its memory is deleted"
	}
	return "its memory is deleted by the Tenant process that holds the fleet (at once while one runs, else when one next starts)"
}

// removeFleetMemory deletes everything the master keeps of a revoked
// machine's memory. The process that holds the fleet's memory does it
// itself; any other leaves a mark for it, deletes at once if no process
// holds the memory, and says which happened.
func removeFleetMemory(cfgDir, dataDir, id string) (string, error) {
	inUse := false
	if h := theFleet.Load(); h != nil {
		h.mu.Lock()
		mem := h.mem
		h.mu.Unlock()
		if mem != nil {
			err := mem.shadows.Remove(id)
			if err == nil {
				mem.forget(id)
				return "its memory is deleted", nil
			}
			if !errors.Is(err, shadow.ErrInUse) {
				return "", err
			}
			inUse = true // a call holds it this moment: the sweep deletes it after
		}
	}
	dir := filepath.Join(fleetShadowDir(dataDir), id)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return "it kept no memory", nil
	}
	if err := fsperm.Replace(filepath.Join(dir, memoryRemoveMark), nil); err != nil {
		return "", fmt.Errorf("its memory could not be marked for deletion: %w", err)
	}
	if inUse {
		return "its memory is deleted within seconds, once no call holds it", nil
	}
	recipient, _, err := fleet.Open(cfgDir).Recovery()
	if err != nil || recipient == "" {
		return "its memory is deleted by the next Tenant process that holds the fleet", nil
	}
	key, err := shadow.OpenKey(fleet.KeyPath(cfgDir))
	if err != nil {
		return "its memory is deleted by the next Tenant process that holds the fleet", nil
	}
	defer key.Close()
	shadows, err := shadow.OpenStore(fleetShadowDir(dataDir), key, recipient, shadow.Options{})
	if errors.Is(err, datalock.ErrBusy) {
		return "its memory is deleted within seconds by the Tenant process that holds the fleet", nil
	}
	if err != nil {
		return "its memory is deleted by the next Tenant process that holds the fleet", nil
	}
	defer shadows.Close()
	if err := shadows.Remove(id); err != nil {
		return "", err
	}
	return "its memory is deleted", nil
}

// checkFleetKeys is doctor's check of a fleet master's keys (TEN-532): the
// fleet key is there, owner-only, opens, and is held in locked memory (or
// why not: a failure to lock stops nothing); and the recovery key was made
// and proven. It reads the key only as the running master does, into locked
// memory, and only when its file is already owner-only, so plain doctor
// changes nothing.
func checkFleetKeys(_ context.Context, e *doctorEnv) checkResult {
	cfg := e.c.cfgDir
	if _, err := os.Lstat(fleet.MasterIdentityPath(cfg)); err != nil {
		return checkResult{Status: statusSkip, Detail: "this Tenant is not a fleet master"}
	}
	status, notes, fix := statusOK, []string{}, ""
	worse := func(s checkStatus, note, todo string) {
		if s > status {
			status = s
		}
		notes = append(notes, note)
		if fix == "" {
			fix = todo
		}
	}
	path := fleet.KeyPath(cfg)
	switch report := secrets.Probe(path); {
	case !report.Exists:
		worse(statusWarn, "no fleet key, so no machine's memory is kept", "tenant fleet init")
	case !report.Protected:
		worse(statusFail, "the fleet key file is not owner-only: "+report.Detail, "")
	default:
		key, err := shadow.OpenKey(path)
		if err != nil {
			worse(statusFail, "the fleet key does not open: "+err.Error(), "")
			break
		}
		locked, why := key.Locked()
		_ = key.Close()
		if locked {
			notes = append(notes, "the fleet key is held in locked memory, kept out of the swap file")
		} else {
			worse(statusWarn, "the fleet key cannot be held in locked memory here ("+why+"); Tenant runs all the same", "")
		}
	}
	switch rec, proven, err := fleet.Open(cfg).Recovery(); {
	case err != nil:
		worse(statusFail, err.Error(), "")
	case rec == "":
		worse(statusWarn, "no recovery key: if the fleet key is lost, every machine's memory is lost with it", "tenant fleet init, at a terminal")
	case proven.IsZero():
		worse(statusWarn, "the recovery key was never proven: nothing shows its identity was saved", "")
	default:
		notes = append(notes, "the recovery key was proven "+fleetTime(proven))
	}
	return checkResult{Status: status, Detail: strings.Join(notes, "; "), Fix: fix}
}
