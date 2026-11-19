package main

// fleetforget.go: what the process that holds the fleet does to its
// machines' memory besides keeping it (TEN-532; docs/plans/gap6-fleet-plan.md,
// section 6): a forget, the inventory taken at confirmation, and
// distillation over each machine's own episodes.
//
// Forget. `tenant privacy forget` runs in a process of its own, which may not
// hold the fleet. It leaves a request beside the machines' memory, sealed to
// the fleet key and the recovery key, since it names what is to be
// forgotten. The process that holds the fleet takes each request into its
// record of forgets (sealed the same way) and, machine by machine:
//
//  1. deletes from the machine's shadow what the forget selects
//     (privacy.ForgetOpen, on the shadow's own databases), and the shadows
//     kept aside, which still hold it;
//  2. makes a new generation, and records it as the one that holds the
//     forget;
//  3. once the machine is online and holds that generation (the next
//     comparison pushes it), tells it to drop every older copy: the one case
//     where a child deletes. A machine that is offline is told when it is
//     back.
//
// A shadow later replaced by a machine's copy that is no newer than that
// generation (a rebuild, or the child winning after a restore) has the
// forget applied again and goes through 2 and 3 again: a forget survives a
// rebuild. Copies a machine keeps under another fleet key are not its
// master's to delete; the operator is told.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/fleet/shadow"
	"tenant/internal/fsperm"
	"tenant/internal/improve"
	"tenant/internal/memory/distill"
	"tenant/internal/memory/episodic"
	"tenant/internal/privacy"
)

const (
	fleetForgetsFile  = "forgets.age" // the holder's record of forgets, sealed
	fleetForgetPrefix = "forget-"     // a request: forget-<unix nano>.age, sealed
	fleetForgetsMax   = 8 << 20       // bytes of the record, opened
	inventoryMark     = "inventory"   // in a machine's directory: take its inventory

	fleetForgetListMax = 10000 // sessions or runs in one forget
	fleetForgetTextMax = 4096
)

var fleetMachineID = regexp.MustCompile(`^m[0-9a-f]{16}$`)

// fleetForget is one forget as it applies to the machines' memory.
type fleetForget struct {
	At       time.Time            `json:"at"`
	Sessions []string             `json:"sessions,omitempty"`
	Runs     []string             `json:"runs,omitempty"`
	Match    string               `json:"match,omitempty"`
	Before   time.Time            `json:"before,omitzero"`
	Agent    string               `json:"agent,omitempty"`
	Machines map[string]*forgetOn `json:"machines,omitempty"`
}

// forgetOn is where one forget stands on one machine.
type forgetOn struct {
	Applied bool   `json:"applied,omitempty"` // deleted from its shadow
	Gen     uint64 `json:"gen,omitempty"`     // the generation that holds it; 0: none was made
	Purged  bool   `json:"purged,omitempty"`  // the machine was told to drop its older copies
	Foreign bool   `json:"foreign,omitempty"` // it keeps copies under another key, which it was not
}

func (f *fleetForget) selector() privacy.Selector {
	return privacy.Selector{Sessions: f.Sessions, Runs: f.Runs, Match: f.Match, Before: f.Before, Agent: f.Agent}
}

// parseFleetForgets reads the holder's record or a request's one forget:
// JSON, every field checked. It is the only reader of both.
func parseFleetForgets(data []byte) ([]*fleetForget, error) {
	var all []*fleetForget
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("fleet: the record of forgets: %w", err)
	}
	short := func(list []string) bool {
		if len(list) > fleetForgetListMax {
			return false
		}
		for _, s := range list {
			if s == "" || len(s) > 256 {
				return false
			}
		}
		return true
	}
	for i, f := range all {
		switch {
		case f == nil:
			return nil, fmt.Errorf("fleet: the record of forgets: entry %d is empty", i)
		case !short(f.Sessions) || !short(f.Runs) || len(f.Match) > fleetForgetTextMax || len(f.Agent) > 256:
			return nil, fmt.Errorf("fleet: the record of forgets: entry %d is too large", i)
		case f.selector().Empty():
			return nil, fmt.Errorf("fleet: the record of forgets: entry %d selects nothing", i)
		}
		for id, on := range f.Machines {
			if !fleetMachineID.MatchString(id) || on == nil {
				return nil, fmt.Errorf("fleet: the record of forgets: entry %d names %q, which is not a machine id", i, id)
			}
		}
	}
	return all, nil
}

// fleetForgetSelector is the part of a forget of the master's memory that a
// machine's memory has: sessions, text, a date and an agent as they are, and
// episodes by the runs they were recorded in, since a machine's extract of a
// turn has the master episode's run and not its id. Facts and skills by id,
// and archive months, are the master's alone. It must run before the
// master's episodes are deleted.
func fleetForgetSelector(d privacy.Data, sel privacy.Selector) (privacy.Selector, error) {
	out := privacy.Selector{Sessions: sel.Sessions, Runs: slices.Clone(sel.Runs), Match: sel.Match, Before: sel.Before, Agent: sel.Agent}
	if len(sel.Episodes) == 0 {
		return out, nil
	}
	st, err := episodic.OpenExisting(filepath.Join(d.DataDir, "episodes.db"))
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer st.Close()
	for _, id := range sel.Episodes {
		ep, err := st.Get(context.Background(), id)
		if errors.Is(err, episodic.ErrNotFound) {
			continue
		}
		if err != nil {
			return out, err
		}
		if ep.SourceRunID != "" && !slices.Contains(out.Runs, ep.SourceRunID) {
			out.Runs = append(out.Runs, ep.SourceRunID)
		}
	}
	return out, nil
}

// recordFleetForget leaves a forget for the process that holds the fleet to
// apply to every machine's memory, and returns what to tell the operator; ""
// when there is no fleet memory here.
func recordFleetForget(cfgDir, dataDir string, sel privacy.Selector) (string, error) {
	if sel.Empty() {
		return "", nil
	}
	if _, err := os.Lstat(fleet.MasterIdentityPath(cfgDir)); err != nil {
		return "", nil
	}
	recipient, _, err := fleet.Open(cfgDir).Recovery()
	if err != nil {
		return "", err
	}
	key, err := shadow.OpenKey(fleet.KeyPath(cfgDir))
	if recipient == "" || errors.Is(err, fs.ErrNotExist) {
		return "", nil // no machine keeps memory yet
	}
	if err != nil {
		return "", fmt.Errorf("the fleet key cannot be opened, so the forget was not passed to the machines' memory: %w", err)
	}
	defer key.Close()
	raw, err := json.Marshal([]*fleetForget{{At: time.Now().UTC(), Sessions: sel.Sessions, Runs: sel.Runs, Match: sel.Match, Before: sel.Before, Agent: sel.Agent}})
	if err != nil {
		return "", err
	}
	blob, err := key.Seal(raw, recipient)
	if err != nil {
		return "", err
	}
	dir := fleetShadowDir(dataDir)
	if err := fsperm.MkdirAll(dir); err != nil {
		return "", err
	}
	name := fleetForgetPrefix + strconv.FormatInt(time.Now().UnixNano(), 10) + ".age"
	if err := fsperm.Replace(filepath.Join(dir, name), blob); err != nil {
		return "", err
	}
	return "The same is forgotten from every fleet machine's memory by the Tenant process that holds the fleet (at once while one runs, else when one next starts): " +
		"its shadow on this master, then a new copy on the machine, which is told to delete its older copies; a machine that is offline is told when it is back. " +
		"Copies a machine keeps under an earlier fleet key cannot be deleted from here: the operator deletes them on that machine.", nil
}

// --- in the process that holds the fleet ---

func (m *fleetMemory) forgetsPath() string {
	return filepath.Join(fleetShadowDir(m.dataDir), fleetForgetsFile)
}

// loadForgets reads the record of forgets when the memory opens. A record
// that cannot be read is moved aside, never written over, and the operator
// is told.
func (m *fleetMemory) loadForgets() {
	blob, err := os.ReadFile(m.forgetsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var all []*fleetForget
	if err == nil {
		var data []byte
		if data, err = m.key.Unseal(blob, fleetForgetsMax); err == nil {
			all, err = parseFleetForgets(data)
		}
	}
	if err != nil {
		aside := m.forgetsPath() + ".unreadable-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		_ = os.Rename(m.forgetsPath(), aside)
		m.log.Warn("fleet: the record of forgets cannot be read; it was moved aside", "file", aside, "err", err)
		m.tell("fleet: the record of past forgets cannot be read (" + printableDetail(err.Error()) + "); it was moved aside to " + aside +
			". A machine's memory rebuilt from an old copy will not have those forgets applied again: forget the same again with tenant privacy forget.")
		return
	}
	m.fgMu.Lock()
	m.forgets = all
	m.fgMu.Unlock()
}

// saveForgets writes the record. The caller holds fgMu.
func (m *fleetMemory) saveForgets() error {
	raw, err := json.Marshal(m.forgets)
	if err != nil {
		return err
	}
	blob, err := m.key.Seal(raw, m.recovery)
	if err != nil {
		return err
	}
	return fsperm.Replace(m.forgetsPath(), blob)
}

// takeForgetRequests takes the requests other processes left into the
// record, for every machine of the fleet now, and removes them.
func (m *fleetMemory) takeForgetRequests() {
	dir := fleetShadowDir(m.dataDir)
	entries, _ := os.ReadDir(dir)
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, fleetForgetPrefix) || !strings.HasSuffix(name, ".age") {
			continue
		}
		path := filepath.Join(dir, name)
		blob, err := os.ReadFile(path)
		var got []*fleetForget
		if err == nil {
			var data []byte
			if data, err = m.key.Unseal(blob, fleetForgetsMax); err == nil {
				got, err = parseFleetForgets(data)
			}
		}
		if err != nil {
			_ = os.Rename(path, path+".unreadable")
			m.tell("fleet: a forget left for the machines' memory cannot be read (" + printableDetail(err.Error()) + "); run it again with tenant privacy forget")
			continue
		}
		if ids == nil {
			ids = m.everyMachine()
		}
		m.fgMu.Lock()
		for _, f := range got {
			f.Machines = map[string]*forgetOn{}
			for _, id := range ids {
				f.Machines[id] = &forgetOn{}
			}
			m.forgets = append(m.forgets, f)
		}
		err = m.saveForgets()
		m.fgMu.Unlock()
		if err != nil {
			m.log.Warn("fleet: a forget could not be recorded; it is tried again", "err", err)
			m.fgMu.Lock()
			m.forgets = m.forgets[:len(m.forgets)-len(got)]
			m.fgMu.Unlock()
			continue
		}
		_ = os.Remove(path)
	}
}

// everyMachine is every machine whose memory a forget reaches: the ones
// fleet.json has, and any whose memory is still on disk.
func (m *fleetMemory) everyMachine() []string {
	var ids []string
	if all, err := m.mgr.Machines(); err == nil {
		for _, st := range all {
			if st.State != fleet.StateRevoked {
				ids = append(ids, st.ID)
			}
		}
	}
	entries, _ := os.ReadDir(fleetShadowDir(m.dataDir))
	for _, e := range entries {
		if e.IsDir() && fleetMachineID.MatchString(e.Name()) && !slices.Contains(ids, e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// applyForgets applies every forget a machine has not had yet. A machine
// whose memory cannot be opened now keeps it pending.
func (m *fleetMemory) applyForgets(ctx context.Context) {
	m.fgMu.Lock()
	defer m.fgMu.Unlock()
	changed := false
	for _, f := range m.forgets {
		for id, on := range f.Machines {
			if on.Applied {
				continue
			}
			if err := m.applyForget(ctx, id, f, on, true); err != nil {
				m.log.Debug("fleet: a forget waits for a machine's memory", "machine", id, "err", err)
				continue
			}
			changed = true
		}
	}
	if changed {
		if err := m.saveForgets(); err != nil {
			m.log.Warn("fleet: the record of forgets was not written", "err", err)
		}
	}
}

// applyForget deletes what f selects from machine id's shadow and makes the
// generation that holds it. The caller holds fgMu.
func (m *fleetMemory) applyForget(ctx context.Context, id string, f *fleetForget, on *forgetOn, aside bool) error {
	// A memory that does not load waits: the comparisons rebuild it or mark
	// it as needing the recovery key, and the forget is applied after.
	p, err := m.shadows.Acquire(ctx, id)
	if err != nil {
		return err
	}
	res, err := privacy.ForgetOpen(ctx, p.Episodic.DB(), p.Semantic.DB(), f.selector(), time.Now())
	p.Release()
	if err != nil {
		return err
	}
	if aside {
		// What was set aside before the forget still holds what it removed.
		if err := m.shadows.RemoveAside(id); err != nil {
			return err
		}
	}
	c, _, err := m.shadows.Mint(ctx, id)
	if err != nil {
		return err
	}
	removed := 0
	for _, it := range res.Items {
		if it.Action == "delete" {
			removed += it.Count
		}
	}
	*on = forgetOn{Applied: true, Gen: c.Gen, Purged: c.Gen == 0}
	m.log.Info("fleet: a forget was applied to a machine's memory", "machine", id, "removed", removed, "generation", c.Gen)
	if c.Gen > 0 {
		m.kick(id) // the new generation goes to the machine, and then the purge
	}
	return nil
}

// reapplyForgets is a shadow replaced by a copy of generation gen: every
// forget that copy may predate is applied again.
func (m *fleetMemory) reapplyForgets(ctx context.Context, id string, gen uint64) {
	m.fgMu.Lock()
	defer m.fgMu.Unlock()
	changed := false
	for _, f := range m.forgets {
		on := f.Machines[id]
		if on == nil || !on.Applied || (on.Gen != 0 && gen > on.Gen) {
			continue
		}
		if err := m.applyForget(ctx, id, f, on, false); err != nil {
			on.Applied = false // the sweep tries again
			m.log.Warn("fleet: a forget could not be applied again to a rebuilt memory; it is tried again", "machine", id, "err", err)
		} else {
			m.log.Info("fleet: a forget was applied again to a memory rebuilt from an older copy", "machine", id, "from", gen)
		}
		changed = true
	}
	if changed {
		if err := m.saveForgets(); err != nil {
			m.log.Warn("fleet: the record of forgets was not written", "err", err)
		}
	}
}

// purgeIfOwed tells a machine that holds its master's newest generation to
// drop its older copies, once for every forget it has not been told of.
func (m *fleetMemory) purgeIfOwed(ctx context.Context, id string) {
	keep, _, err := m.shadows.Newest(id)
	if err != nil {
		return
	}
	m.fgMu.Lock()
	owed := false
	for _, f := range m.forgets {
		if on := f.Machines[id]; on != nil && on.Applied && !on.Purged && on.Gen <= keep.Gen {
			owed = true
		}
	}
	m.fgMu.Unlock()
	if !owed {
		return
	}
	if err := m.mgr.MemoryPurge(ctx, id, keep); err != nil {
		m.log.Warn("fleet: a machine was not told to drop its older copies after a forget; it is told at the next comparison", "machine", id, "err", err)
		return
	}
	foreign := false
	if copies, err := m.mgr.MemoryList(ctx, id); err == nil {
		for _, c := range copies {
			foreign = foreign || c.KeyID != keep.KeyID
		}
	}
	m.fgMu.Lock()
	told := false
	for _, f := range m.forgets {
		if on := f.Machines[id]; on != nil && on.Applied && !on.Purged && on.Gen <= keep.Gen {
			told = told || (foreign && !on.Foreign)
			on.Purged, on.Foreign = true, foreign
		}
	}
	err = m.saveForgets()
	m.fgMu.Unlock()
	if err != nil {
		m.log.Warn("fleet: the record of forgets was not written", "err", err)
	}
	m.log.Info("fleet: a machine dropped the copies of its memory older than a forget", "machine", id, "kept", keep.Gen)
	if told {
		m.tell(fmt.Sprintf("fleet: machine %s keeps copies of its memory made under another fleet key. A forget cannot delete them from the master: delete them on the machine (the memory folder in its child directory) if they may hold what was forgotten.", id))
	}
}

// --- the inventory ---

// markInventory asks the process that holds the fleet to record machine
// id's inventory: confirmMachine may run in another process.
func markInventory(dataDir, id string) error {
	dir := filepath.Join(fleetShadowDir(dataDir), id)
	if err := fsperm.MkdirAll(dir); err != nil {
		return err
	}
	return fsperm.Replace(filepath.Join(dir, inventoryMark), nil)
}

// takeInventory records the inventory of machine id, as confirmation left
// it to, from the fields fleet.json keeps of what the machine reported.
func (m *fleetMemory) takeInventory(ctx context.Context, id string) {
	mark := filepath.Join(fleetShadowDir(m.dataDir), id, inventoryMark)
	mach, err := m.store.Machine(id)
	if err != nil || mach.State == fleet.StateRevoked || mach.State == fleet.StatePending {
		_ = os.Remove(mark)
		return
	}
	if err := m.writeFacts(ctx, id, originInventory, fleetInventoryFacts(mach, time.Now())); err != nil {
		m.log.Debug("fleet: a machine's inventory waits", "machine", id, "err", err)
		return
	}
	_ = os.Remove(mark)
	m.log.Info("fleet: a machine's inventory was recorded in its memory", "machine", id)
}

// --- distillation, machine by machine ---

// fleetDistillBound is the longest one machine's distillation holds its
// memory: while it is held the memory is not replaced, and a comparison
// that wants to replace it waits for its next pass.
var fleetDistillBound = 2 * time.Minute

// fleetDistillJob runs the master's distillation over each machine's own
// episodes, into that machine's own facts: the machines whose memory is in
// RAM, one at a time. What it distills is drawn from a machine's output, so
// its facts are recalled as that machine's (their origin is distill).
type fleetDistillJob struct{ d *distill.Distiller }

func (fleetDistillJob) Name() string { return "fleet-distill" }

func (j fleetDistillJob) Run(ctx context.Context) (improve.JobResult, error) {
	mem, err := fleetMem()
	if err != nil {
		return improve.JobResult{Summary: "no machine's memory is open in this process"}, nil
	}
	out := improve.JobResult{Details: map[string]any{}}
	var errs []error
	facts := 0
	for _, id := range mem.shadows.Loaded() {
		res, err := mem.distill(ctx, id, j.d)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		if res != nil {
			facts += res.FactsInserted + res.FactsReaffirmed
			out.SourceRuns = append(out.SourceRuns, res.SourceRunIDs...)
			out.Details[id] = res.EpisodesProcessed
		}
	}
	out.Changed = facts > 0
	out.Summary = fmt.Sprintf("%d machines' memory distilled, %d facts", len(out.Details), facts)
	return out, errors.Join(errs...)
}

// distill runs base over machine id's episodes since its own cursor, kept in
// its own memory (so it follows the memory through a rebuild).
func (m *fleetMemory) distill(ctx context.Context, id string, base *distill.Distiller) (*distill.RunResult, error) {
	ctx, cancel := context.WithTimeout(ctx, fleetDistillBound)
	defer cancel()
	p, err := m.shadows.Acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	defer p.Release()
	db := p.Episodic.DB()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS distill_cursor (agent TEXT PRIMARY KEY, last INTEGER NOT NULL)`); err != nil {
		return nil, err
	}
	var cursor int64
	_ = db.QueryRowContext(ctx, `SELECT last FROM distill_cursor WHERE agent = ?`, base.AgentID).Scan(&cursor)
	d := *base
	d.Episodic, d.Semantic = p.Episodic, p.Semantic
	res, err := d.Run(ctx, cursor)
	if res != nil && res.LastEpisodeID > cursor {
		if _, werr := db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO distill_cursor (agent, last) VALUES (?, ?) ON CONFLICT(agent) DO UPDATE SET last = excluded.last`,
			base.AgentID, res.LastEpisodeID); werr != nil {
			err = errors.Join(err, werr)
		}
	}
	return res, err
}
