package main

// fleetupdate.go (TEN-533; docs/plans/gap6-fleet-plan.md, section 8): `tenant
// fleet update`, the master's half. Updating a machine is an operation on its
// link, not a command run there, and it is allowed whatever link version the
// machine speaks (fleet/update never changes shape).
//
// It runs in the process that holds the fleet, since only that process has
// the links: the dashboard's POST /fleet/update starts it there (the command
// line posts to it, then reads the progress from GET /api/fleet). For one
// machine:
//
//  1. Refuse what cannot go: a machine that is not confirmed or not online
//     (nothing is queued), one whose processor is not known, an older
//     version than it runs unless asked.
//  2. Fetch the release's archive for the machine's OS and processor and
//     verify it exactly as `tenant update` verifies its own (fetchRelease):
//     the checksum always, the signature when it can.
//  3. Send it in pieces with its SHA-256 (fleet.Manager.Update), from where
//     the machine says it got to.
//  4. Wait for the machine to say, in a hello on a new link, that the new
//     version reached the master (done), or that it put the old one back
//     (rolled back, and why). The child does the rest (childupdate.go).

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/selfupdate"
)

var (
	// fleetReleaseAPI is where releases are looked up: "" is GitHub's API.
	// Tests point it at a server of their own.
	fleetReleaseAPI = ""
	// fleetUpdateWait is how long the master waits, once the machine is
	// applying, for it to say how the update went: the time the child gives
	// its running calls, twice its own limit on each of its two starts, and
	// room for the restarts. Calls to the machine are kept back as long.
	fleetUpdateWait = fleet.UpdateHold
	// fleetUpdatePoll is how often the master looks while it waits.
	fleetUpdatePoll = 500 * time.Millisecond
	// fleetPieceWait is the longest a machine is waited on for its answer
	// to one piece: a machine that stalls is given up on, not waited out.
	fleetPieceWait = 30 * time.Second
)

// The stages of one machine's update, as the page and the command line show
// them. The last four are where it ends.
const (
	fleetUpdFetching   = "fetching"
	fleetUpdSending    = "sending"
	fleetUpdApplying   = "applying"
	fleetUpdDone       = "done"
	fleetUpdRolledBack = "rolled_back"
	fleetUpdFailed     = "failed"
	fleetUpdRefused    = "refused"
)

// fleetUpdateOpts is what the operator asked for.
type fleetUpdateOpts struct {
	version    string // vX.Y.Z; "" is the latest release
	allowOlder bool   // send a version older than the machine runs (or one that cannot be compared)
	arch       string // amd64 | arm64, for a machine too old to say
	// build is an archive on this computer to send in place of a release
	// (--build; TEN-552): a development build, named as a release's archive is
	// (scripts/devbuild writes them). Its name says its version, system and
	// processor. No release vouches for it, so only a machine set to trust
	// its master's hash takes it.
	build string
}

// buildNamed is what the name of a build's archive has to be.
const buildNamed = "tenant_<version>_<system>_<processor>.tar.gz, or .zip for Windows, as `go run ./scripts/devbuild` writes it"

// versionArg is the form --version takes.
var versionArg = regexp.MustCompile(`^v?[0-9]{1,9}(\.[0-9]{1,9}){1,3}(-[0-9A-Za-z.]{1,32})?$`)

// fleetUpdateState is one machine's update, as far as it has got. Lines are
// what the operator is told, in order.
type fleetUpdateState struct {
	Machine  string
	From, To string
	Stage    string
	Lines    []string
	Started  time.Time
	Finished time.Time
}

// Over reports whether the update has ended, one way or another.
func (s fleetUpdateState) Over() bool { return !s.Finished.IsZero() }

// fleetUpdates are the updates this process has run, one per machine: the
// last one, kept until the next.
type fleetUpdates struct {
	dataDir string
	fetch   sync.Mutex        // one download at a time: machines of one kind share it
	fetched map[string]string // archive path -> the SHA-256 the release listed for it, as verified when it was fetched

	mu   sync.Mutex
	runs map[string]*fleetUpdateState
}

func newFleetUpdates(dataDir string) *fleetUpdates {
	return &fleetUpdates{dataDir: dataDir, runs: map[string]*fleetUpdateState{}, fetched: map[string]string{}}
}

// State is a copy of a machine's last update; ok is false when there was none.
func (u *fleetUpdates) State(machine string) (fleetUpdateState, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.runs[machine]
	if !ok {
		return fleetUpdateState{}, false
	}
	out := *s
	out.Lines = slices.Clone(s.Lines)
	return out, true
}

func (u *fleetUpdates) say(s *fleetUpdateState, stage, line string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if stage != "" {
		s.Stage = stage
	}
	if line != "" && len(s.Lines) < 32 {
		s.Lines = append(s.Lines, line)
	}
	if line != "" { // the fleet log keeps every step, past what the page shows
		step := fleet.UpdateStep
		switch s.Stage {
		case fleetUpdRolledBack, fleetUpdFailed, fleetUpdRefused:
			step = fleet.UpdateNotDone
		}
		step.Log(context.Background(), "name", fleetNames([]string{s.Machine})[0], "machine", s.Machine, "stage", s.Stage, "line", line, "from", s.From, "to", s.To)
	}
	switch s.Stage {
	case fleetUpdDone, fleetUpdRolledBack, fleetUpdFailed, fleetUpdRefused:
		s.Finished = time.Now()
	}
}

// Start begins updating machine, or says at once why it will not: the
// refusals that need no network. The rest is reported in the machine's state.
func (u *fleetUpdates) Start(mgr *fleet.Manager, machine string, o fleetUpdateOpts) error {
	if o.version != "" && !versionArg.MatchString(o.version) {
		return fmt.Errorf("%s is not a version (vX.Y.Z)", fleetQuote(o.version))
	}
	if o.arch != "" && o.arch != "amd64" && o.arch != "arm64" {
		return fmt.Errorf("--arch is amd64 or arm64, not %s", fleetQuote(o.arch))
	}
	if o.build != "" {
		switch _, _, _, named := selfupdate.ParseAssetName(filepath.Base(o.build)); {
		case o.version != "":
			return errors.New("a build is sent as it is: --build and --version do not go together")
		case !filepath.IsAbs(o.build):
			return fmt.Errorf("the build %s is not a full path on this computer", fleetQuote(o.build))
		case !named:
			return fmt.Errorf("%s is not a build's archive by its name, which says what it is: %s", fleetQuote(filepath.Base(o.build)), buildNamed)
		}
		if fi, err := os.Stat(o.build); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("the build %s is not a file this Tenant can read", fleetQuote(o.build))
		}
	}
	all, err := mgr.Machines()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(all, func(st fleet.MachineStatus) bool { return st.ID == machine })
	if i < 0 {
		return fleet.ErrUnknown
	}
	st := all[i]
	switch {
	case st.State != fleet.StateConfirmed:
		return fmt.Errorf("%s is %s: only a confirmed machine is updated", fleetName(st.Name), st.State)
	case !st.Online:
		since := "it has not connected since this master started"
		if !st.Since.IsZero() {
			since = "offline since " + fleetTime(st.Since)
		}
		return fmt.Errorf("%s is offline (%s). Nothing is queued: run the update again once it is back", fleetName(st.Name), since)
	}
	u.mu.Lock()
	if cur := u.runs[machine]; cur != nil && !cur.Over() {
		u.mu.Unlock()
		return fmt.Errorf("%s is being updated already", fleetName(st.Name))
	}
	s := &fleetUpdateState{Machine: machine, From: st.Reported.Version, Stage: fleetUpdFetching, Started: time.Now()}
	u.runs[machine] = s
	u.mu.Unlock()
	go u.run(mgr, st, o, s)
	return nil
}

// run is one machine's update, after Start. It ends in one of the last four
// stages, with a line that says why.
func (u *fleetUpdates) run(mgr *fleet.Manager, st fleet.MachineStatus, o fleetUpdateOpts, s *fleetUpdateState) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute+fleetUpdateWait)
	defer cancel()
	fail := func(stage, line string) { u.say(s, stage, line) }
	if o.build != "" {
		u.runBuild(ctx, mgr, st, o, s)
		return
	}
	repo := updateRepo()
	if repo == "" {
		fail(fleetUpdRefused, "no release source: this build names no repository (TENANT_UPDATE_REPO=owner/name sets one)")
		return
	}
	arch := st.Arch
	if arch == "" {
		arch = o.arch
	}
	if arch == "" {
		fail(fleetUpdRefused, "the machine does not say which processor it has (it runs a Tenant from before it did): give one with --arch amd64 or --arch arm64")
		return
	}
	goos := st.Reported.OS
	client := &http.Client{Timeout: 10 * time.Minute}
	gh := os.Getenv("GITHUB_TOKEN")
	var rel selfupdate.Release
	var err error
	if o.version != "" {
		tag := o.version
		if tag[0] != 'v' {
			tag = "v" + tag
		}
		rel, err = selfupdate.ByTag(ctx, client, fleetReleaseAPI, repo, tag, gh)
	} else {
		rel, err = selfupdate.Latest(ctx, client, fleetReleaseAPI, repo, gh)
	}
	if err == nil && !versionArg.MatchString(rel.Tag) {
		err = fmt.Errorf("the release's tag %s is not a version", fleetQuote(rel.Tag))
	}
	if err != nil {
		fail(fleetUpdFailed, "the release could not be looked up: "+err.Error())
		return
	}
	newV := selfupdate.VersionOf(rel.Tag)
	u.mu.Lock()
	s.To = newV
	u.mu.Unlock()
	switch c, ok := selfupdate.Compare(newV, st.Reported.Version); {
	case ok && c == 0:
		fail(fleetUpdDone, "already runs "+newV+", by its own account: nothing was sent")
		return
	case (!ok || c < 0) && !o.allowOlder:
		what := "older than the " + fleetQuote(st.Reported.Version) + " it runs"
		if !ok {
			what = "not comparable with the " + fleetQuote(st.Reported.Version) + " it says it runs"
		}
		fail(fleetUpdRefused, newV+" is "+what+": nothing was sent. To send it anyway, ask again with --allow-older")
		return
	}
	u.say(s, "", fmt.Sprintf("fetching %s for %s/%s", rel.Tag, goos, arch))
	archive, sum, said, err := u.fetchFor(ctx, client, repo, gh, rel, goos, arch)
	if err != nil {
		fail(fleetUpdFailed, "the release could not be fetched and verified: "+err.Error()+". Nothing was sent")
		return
	}
	var size int64
	if fi, serr := os.Stat(archive); serr == nil {
		size = fi.Size()
	} else {
		err = serr
	}
	if err != nil {
		fail(fleetUpdFailed, "the fetched archive cannot be read: "+err.Error())
		return
	}
	u.say(s, fleetUpdSending, fmt.Sprintf("fetched %s (%s): %s", filepath.Base(archive), fleetMB(size), said))
	// The release's own material goes with it (decision A71): a machine on
	// the default rule verifies the release itself, and refuses on the first
	// piece, before the archive is sent, if it cannot.
	checksums, bundle, signature, err := releaseMaterial(filepath.Dir(archive))
	if err != nil {
		fail(fleetUpdFailed, err.Error()+". Nothing was sent")
		return
	}
	u.send(ctx, mgr, st, s, archive, fleet.Update{Version: newV, SHA256: sum, Size: size,
		Asset: filepath.Base(archive), Checksums: checksums, Bundle: bundle, Signature: signature}, "")
}

// runBuild is run for a build handed to this master (--build) in place of a
// release: nothing is fetched and nothing is verified here, since nothing
// vouches for a build but the operator who sends it. Its name says what it
// is; a build for another system or processor than the machine's is refused
// before anything is sent, and the machine itself checks that the binary runs
// there and says the version it was sent as.
func (u *fleetUpdates) runBuild(ctx context.Context, mgr *fleet.Manager, st fleet.MachineStatus, o fleetUpdateOpts, s *fleetUpdateState) {
	fail := func(stage, line string) { u.say(s, stage, line) }
	name := filepath.Base(o.build)
	newV, goos, arch, ok := selfupdate.ParseAssetName(name)
	if !ok {
		fail(fleetUpdRefused, fleetQuote(name)+" is not a build's archive by its name: "+buildNamed+". Nothing was sent")
		return
	}
	u.mu.Lock()
	s.To = newV
	u.mu.Unlock()
	switch has := st.Arch; {
	case goos != st.Reported.OS || has != "" && arch != has:
		if has == "" {
			has = "a processor it does not say"
		}
		fail(fleetUpdRefused, fmt.Sprintf("%s is a build for %s/%s, and the machine is %s/%s: nothing was sent", name, goos, arch, fleetQuote(st.Reported.OS), has))
		return
	case newV == st.Reported.Version:
		fail(fleetUpdDone, "already runs "+newV+", by its own account: nothing was sent")
		return
	}
	fi, err := os.Stat(o.build)
	var sum string
	if err == nil && fi.Size() == 0 {
		err = errors.New("it is empty")
	}
	if err == nil {
		sum, err = selfupdate.SHA256(o.build)
	}
	if err != nil {
		fail(fleetUpdFailed, "the build cannot be read: "+err.Error()+". Nothing was sent")
		return
	}
	u.say(s, fleetUpdSending, fmt.Sprintf("sending %s (%s): a build this Tenant was handed, which no release vouches for", name, fleetMB(fi.Size())))
	u.send(ctx, mgr, st, s, o.build, fleet.Update{Version: newV, SHA256: sum, Size: fi.Size()},
		"A machine takes a build of your own only when it is set to trust its master's hash: `tenant child updates master`, run on that machine")
}

// send is the second half of a machine's update, a release's or a build's:
// the archive goes over the link in pieces, and the machine is waited on to
// say how the update went. unverified, when there is one, is said besides when
// the machine refuses the archive on its first piece: what a machine that
// verifies releases needs to be told about one that is not a release.
func (u *fleetUpdates) send(ctx context.Context, mgr *fleet.Manager, st fleet.MachineStatus, s *fleetUpdateState, archive string, upd fleet.Update, unverified string) {
	fail := func(stage, line string) { u.say(s, stage, line) }
	newV, size := upd.Version, upd.Size
	sent, from, opening, err := sendUpdate(ctx, mgr, st.ID, archive, upd)
	if opening.Rule != "" || err == nil {
		u.say(s, "", ruleWords(opening.Rule, opening.Verified))
	}
	resumed := ""
	if from > 0 {
		resumed = fmt.Sprintf(" (the machine had %s of it from an earlier try)", fleetMB(from))
	}
	applied := time.Now()
	switch {
	case errors.Is(err, fleet.ErrOutcomeUnknown) && sent == size:
		// The link went with the last piece's answer: the machine may well
		// be applying it. Its next hello says.
		u.say(s, fleetUpdApplying, fmt.Sprintf("sent %s%s; the link dropped with the last piece, so the machine may be applying it", fleetMB(size), resumed))
	case errors.Is(err, fleet.ErrOffline), errors.Is(err, fleet.ErrOutcomeUnknown):
		fail(fleetUpdFailed, fmt.Sprintf("the machine went offline during the transfer, after %s of %s. Nothing changed there: a machine applies an update only once all of it has arrived and checked", fleetMB(sent), fleetMB(size)))
		return
	case err != nil:
		line := err.Error()
		if unverified != "" && opening.Rule == "" && sent == 0 {
			line += ". " + unverified
		}
		fail(fleetUpdFailed, line)
		return
	default:
		u.say(s, fleetUpdApplying, fmt.Sprintf("sent %s%s; the machine checked its SHA-256 and is applying it. The calls it is running finish first, and new ones wait until it is back. Its background commands end at the restart", fleetMB(size), resumed))
	}

	trial := false
	for deadline := time.Now().Add(fleetUpdateWait); ; {
		select {
		case <-ctx.Done():
			fail(fleetUpdFailed, "gave up waiting: "+ctx.Err().Error())
			return
		case <-time.After(fleetUpdatePoll):
		}
		now, ok := machineStatus(mgr, st.ID)
		// Only a hello on a link made after the archive was taken speaks of
		// this update.
		if ok && now.Online && now.Since.After(applied) && now.Update != nil && now.Update.To == newV {
			r := now.Update
			switch r.State {
			case fleet.UpdateTrial:
				if !trial {
					trial = true
					u.say(s, "", "the new version reached the master in its trial run; the machine is putting it in place")
				}
			case fleet.UpdateDone:
				if now.Reported.Version != newV {
					fail(fleetUpdFailed, "the machine says the update to "+newV+" is done, but reports version "+fleetQuote(now.Reported.Version)+": what it runs is not known")
					return
				}
				u.mu.Lock()
				s.From = firstNonEmpty(r.From, s.From)
				u.mu.Unlock()
				fail(fleetUpdDone, "back on the new version "+newV)
				return
			case fleet.UpdateRolledBack:
				fail(fleetUpdRolledBack, "rolled back to "+fleetQuote(firstNonEmpty(now.Reported.Version, r.From))+": "+fleetSaid(r.Reason))
				return
			}
		}
		if time.Now().After(deadline) {
			fail(fleetUpdFailed, fmt.Sprintf("the machine has not said within %s whether it runs %s or rolled back; `tenant fleet show %s` shows where it stands", fleetUpdateWait, newV, st.Name))
			return
		}
	}
}

func machineStatus(mgr *fleet.Manager, id string) (fleet.MachineStatus, bool) {
	all, err := mgr.Machines()
	if err != nil {
		return fleet.MachineStatus{}, false
	}
	for _, st := range all {
		if st.ID == id {
			return st, true
		}
	}
	return fleet.MachineStatus{}, false
}

// fetchFor is the release's archive for one target, verified, in the data
// dir, and the SHA-256 the release lists for it: the hash the machine is
// sent and checks what arrives against, so a file changed on this disk after
// it was verified is refused there. A second machine of the same kind takes
// the archive already fetched while it still hashes to that.
func (u *fleetUpdates) fetchFor(ctx context.Context, client *http.Client, repo, gh string, rel selfupdate.Release, goos, arch string) (archive, sum, said string, err error) {
	u.fetch.Lock()
	defer u.fetch.Unlock()
	dir := filepath.Join(u.dataDir, "fleet-updates", rel.Tag, goos+"_"+arch) // the tag matched versionArg
	archive = filepath.Join(dir, selfupdate.AssetName(selfupdate.VersionOf(rel.Tag), goos, arch))
	if want := u.fetched[archive]; want != "" {
		if got, err := selfupdate.SHA256(archive); err == nil && got == want {
			return archive, want, "fetched and verified for another machine; it still hashes to what the release lists", nil
		}
		delete(u.fetched, archive)
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", "", err
	}
	archive, said, err = fetchRelease(ctx, client, repo, gh, rel, goos, arch, dir)
	if err == nil {
		var f *os.File
		if f, err = os.Open(filepath.Join(dir, "checksums.txt")); err == nil {
			sum = selfupdate.ParseChecksums(io.LimitReader(f, 1<<20))[filepath.Base(archive)]
			_ = f.Close()
		}
	}
	if err == nil && sum == "" {
		err = errors.New("the release's checksums list no hash for " + filepath.Base(archive))
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", "", err
	}
	u.fetched[archive] = sum
	return archive, sum, said, nil
}

// releaseMaterial is the release's checksums.txt and, when the release has
// them, its signature bundle and its project signature (TEN-542), as
// fetchRelease left them in dir, within the bounds a machine takes.
func releaseMaterial(dir string) (checksums, bundle, signature []byte, err error) {
	if checksums, err = readReleaseFile(dir, "checksums.txt", fleet.MaxReleaseChecksums, true); err != nil {
		return nil, nil, nil, err
	}
	if bundle, err = readReleaseFile(dir, "checksums.txt.sigstore.json", fleet.MaxReleaseBundle, false); err != nil {
		return nil, nil, nil, err
	}
	signature, err = readReleaseFile(dir, selfupdate.SignatureAsset, fleet.MaxReleaseSignature, false)
	return checksums, bundle, signature, err
}

// readReleaseFile is one file fetchRelease left in dir, refused when it is
// over limit bytes; a file that is not there is nil unless need.
func readReleaseFile(dir, name string, limit int, need bool) ([]byte, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) && !need {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err == nil && len(b) > limit {
		err = fmt.Errorf("the release's %s is larger than Tenant takes (%d bytes at most)", name, limit)
	}
	return b, err
}

// ruleWords says which rule a machine applied to an update, and on the
// default rule which check passed (its ack to the opening piece). A word
// the machine sends that is not one of ours is not repeated.
func ruleWords(rule, verified string) string {
	switch rule {
	case string(fleet.UpdatesRelease):
		switch verified {
		case fleet.VerifiedProjectKey:
			return "the machine verified the release's signature itself, with the project key its own Tenant carries, and takes the archive only if it hashes to what that release lists"
		case fleet.VerifiedCosign:
			return "the machine verified the release's signature itself, the release workflow's signature, against the release its own Tenant was built from, and takes the archive only if it hashes to what that release lists"
		}
		return "the machine verified the release's signature itself, against the release its own Tenant was built from, and takes the archive only if it hashes to what that release lists"
	case string(fleet.UpdatesMaster):
		return "the machine is set to trust this master's hash (`updates master` on the machine): it checks the archive against the SHA-256 sent with it, not against a signature"
	}
	return "the machine runs a Tenant from before machines verified releases: it trusts this master's hash"
}

// sendUpdate sends the archive to the machine in pieces, from where the
// machine says it has got to (from), and returns how much it holds. It ends when
// the machine says it is applying, and fails when the machine stops taking
// pieces or answers what is not about this archive: a machine may stall or
// lie, and is never waited on for longer than a request's limit.
//
// The return is bare on purpose. Written `return sent, from, opening, err`,
// go1.26 on arm64 (seen with 1.26.4 and 1.26.9, on Apple Silicon) stops
// keeping opening alive across the call once this function is inlined into
// its caller: the garbage collector freed the strings of the machine's
// opening answer during the transfer, and the master read other memory in
// their place and reported the wrong rule. What it takes is two results
// whose address is taken, one of them a struct too large for registers,
// returned by name; amd64 is not affected. TestFleetUpdateRealProcesses
// fails on a Mac if this comes back.
func sendUpdate(ctx context.Context, mgr *fleet.Manager, id, path string, u fleet.Update) (sent, from int64, opening fleet.UpdateAck, err error) {
	sent, err = sendPieces(ctx, mgr, id, path, u, &from, &opening)
	return
}

func sendPieces(ctx context.Context, mgr *fleet.Manager, id, path string, u fleet.Update, from *int64, opening *fleet.UpdateAck) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	piece := func(p fleet.Update) (fleet.UpdateAck, error) {
		ctx, cancel := context.WithTimeout(ctx, fleetPieceWait)
		defer cancel()
		return mgr.Update(ctx, id, p)
	}
	ack, err := piece(u) // no data, and the release's material: how far it has got
	if err != nil {
		return 0, updateRefused(err)
	}
	*opening = ack
	// The material went once; the archive's pieces carry only themselves.
	u.Asset, u.Checksums, u.Bundle, u.Signature = "", nil, nil, nil
	if ack.Received > 0 && ack.Received < u.Size && !ack.Applying {
		*from = ack.Received
	}
	buf := make([]byte, fleet.MaxUpdateChunk)
	got, stuck, sent := int64(0), 0, false
	for budget := 2*(u.Size/fleet.MaxUpdateChunk) + 8; ; budget-- {
		switch {
		case ack.Received < 0 || ack.Received > u.Size:
			return got, fmt.Errorf("the machine's answer is not about this archive (it says it holds %d of %d bytes): stopped", ack.Received, u.Size)
		case ack.Applying && ack.Received == u.Size:
			return u.Size, nil
		case ack.Applying:
			return got, errors.New("the machine says it is applying an archive it does not hold all of: stopped")
		case ack.Received == u.Size:
			return u.Size, errors.New("the machine holds the whole archive and did not start applying it")
		case sent && ack.Received <= got:
			if stuck++; stuck > 3 {
				return got, fmt.Errorf("the machine stopped taking the archive at %s of %s", fleetMB(ack.Received), fleetMB(u.Size))
			}
		case budget <= 0:
			return got, errors.New("the machine asked for the archive's pieces more often than it has pieces: stopped")
		}
		got = ack.Received
		n, err := f.ReadAt(buf[:min(int64(len(buf)), u.Size-got)], got)
		if err != nil && !errors.Is(err, io.EOF) {
			return got, err
		}
		next := u
		next.Offset, next.Data, sent = got, buf[:n], true
		if ack, err = piece(next); err != nil {
			if errors.Is(err, fleet.ErrOutcomeUnknown) && got+int64(n) == u.Size {
				return u.Size, err
			}
			return got, updateRefused(err)
		}
	}
}

// updateRefused is a failed fleet/update in the operator's words. What the
// machine said is its own text.
func updateRefused(err error) error {
	switch {
	case errors.Is(err, fleet.ErrOffline), errors.Is(err, fleet.ErrOutcomeUnknown):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("the machine stopped answering during the transfer: stopped. Nothing changed there unless all of the archive had arrived")
	}
	return errors.New("the machine refused the update: " + fleetSaid(err.Error()))
}

func fleetMB(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

// --- the command line ---

// fleetUpdateCmd is `tenant fleet update <name|--all>`. The links are held by
// the running Tenant, so this asks it, as the dashboard's Fleet page does
// (POST /fleet/update with the dashboard's token), and prints each machine's
// progress as GET /api/fleet reports it. Machines are updated one after the
// other; one that fails is reported and the next one goes on.
func fleetUpdateCmd(args []string) error {
	fs := flag.NewFlagSet("fleet update", flag.ContinueOnError)
	all := fs.Bool("all", false, "every confirmed machine, one after the other")
	group := fs.String("group", "", "every member of this group, one after the other")
	ver := fs.String("version", "", "install this release (vX.Y.Z) instead of the latest")
	older := fs.Bool("allow-older", false, "send a version older than the machine runs")
	arch := fs.String("arch", "", "the machine's processor, amd64 or arm64, for a machine too old to say")
	build := fs.String("build", "", "send this archive, a build of your own (go run ./scripts/devbuild), in place of a release")
	hub := fs.String("hub", "", "the running Tenant's dashboard address, when it is not where config.json says")
	cli, err := openFleetCLI(fs, args)
	if err != nil {
		return err
	}
	if _, err := fleetIdentity(cli.c.cfgDir); err != nil {
		return err
	}
	var machines []fleet.Machine
	switch {
	case *all && *group == "" && len(cli.pos) == 0:
		every, err := cli.store.Machines()
		if err != nil {
			return err
		}
		for _, m := range every {
			if m.State == fleet.StateConfirmed {
				machines = append(machines, m)
			}
		}
		if len(machines) == 0 {
			return errors.New("no confirmed machine to update")
		}
	case *group != "" && !*all && len(cli.pos) == 0:
		// Its members by name, each asked of the hub in turn: one that is not
		// confirmed is refused there and counted as not updated.
		if machines, err = cli.ops.targets(fleetSelection{Group: *group}); err != nil {
			return err
		}
		if len(machines) == 0 {
			return fmt.Errorf("group %s has no machine to update", fleetQuote(*group))
		}
	case !*all && *group == "" && len(cli.pos) == 1:
		m, err := cli.ops.machine(cli.pos[0])
		if err != nil {
			return err
		}
		machines = []fleet.Machine{m}
	default:
		return errors.New("usage: tenant fleet update <name> | --all | --group <group> [--version vX.Y.Z | --build ARCHIVE] [--allow-older] [--arch amd64|arm64] [--hub ADDR]")
	}
	o := fleetUpdateOpts{version: *ver, allowOlder: *older, arch: *arch}
	if *build != "" {
		// The running Tenant reads the archive itself, from where it runs: it
		// is told the whole path. What is wrong with it that can be seen from
		// here is said before anything is asked.
		abs, err := filepath.Abs(*build)
		if err != nil {
			return err
		}
		switch _, _, _, named := selfupdate.ParseAssetName(filepath.Base(abs)); {
		case *ver != "":
			return errors.New("a build is sent as it is: --build and --version do not go together")
		case !named:
			return fmt.Errorf("%s is not a build's archive by its name, which says what it is: %s", fleetQuote(filepath.Base(abs)), buildNamed)
		}
		if fi, err := os.Stat(abs); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("there is no build at %s", abs)
		}
		o.build = abs
	}
	token, _ := dashboardClientToken(cli.c.cfgDir, cli.c.lc, true)
	h := fleetHubClient{base: attachBaseURL(*hub, cli.c.lc), token: token, http: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	return h.update(context.Background(), os.Stdout, machines, o)
}

// fleetHubClient is the running Tenant's dashboard, as a command asks it.
type fleetHubClient struct {
	base, token string
	http        *http.Client
}

// update updates each machine in turn and prints what happens. It fails when
// any machine was not updated.
func (h fleetHubClient) update(ctx context.Context, out io.Writer, machines []fleet.Machine, o fleetUpdateOpts) error {
	failed := 0
	for _, m := range machines {
		ok, err := h.updateOne(ctx, out, m, o)
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w", fleetName(m.Name), err)
		case !ok:
			failed++
		}
	}
	switch {
	case failed == 0:
		return nil
	case len(machines) == 1:
		return exitError{code: 1}
	}
	return exitError{1, fmt.Sprintf("%d of %d machines were not updated", failed, len(machines))}
}

// updateOne starts one machine's update on the hub and follows it to its
// end. ok is whether the machine runs the new version (or ran it already);
// err is that the hub could not be asked at all.
func (h fleetHubClient) updateOne(ctx context.Context, out io.Writer, m fleet.Machine, o fleetUpdateOpts) (ok bool, err error) {
	name := fleetName(m.Name)
	form := url.Values{"machine": {m.ID}, "version": {o.version}, "arch": {o.arch}, "build": {o.build}, "confirm": {"yes"}}
	if o.allowOlder {
		form.Set("allow_older", "yes")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/fleet/update", strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("no running Tenant answers at %s, and only the Tenant that holds the fleet (tenant serve) can update a machine: nothing was sent (%v)", h.base, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		return false, fmt.Errorf("the running Tenant at %s answered %s: nothing was sent", h.base, resp.Status)
	}
	if loc, err := url.Parse(resp.Header.Get("Location")); err == nil && loc.Query().Get("err") != "" {
		fmt.Fprintf(out, "%s: refused: %s\n", name, loc.Query().Get("err"))
		return false, nil
	}
	said := 0
	for {
		u, err := h.progress(ctx, m.ID)
		if err != nil {
			return false, err
		}
		if u == nil {
			return false, errors.New("the running Tenant took the update and then lost it")
		}
		for ; said < len(u.Lines); said++ {
			fmt.Fprintf(out, "%s: %s\n", name, u.Lines[said])
		}
		if !u.Running {
			if u.Stage != fleetUpdDone {
				fmt.Fprintf(out, "%s: %s\n", name, strings.ReplaceAll(u.Stage, "_", " "))
			}
			return u.Stage == fleetUpdDone, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(fleetUpdatePoll):
		}
	}
}

// hubUpdate is a machine's update as GET /api/fleet shows it.
type hubUpdate struct {
	Stage   string   `json:"stage"`
	Running bool     `json:"running"`
	Lines   []string `json:"lines"`
}

func (h fleetHubClient) progress(ctx context.Context, id string) (*hubUpdate, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/api/fleet", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the running Tenant stopped answering (%v); the update may go on there: tenant fleet show", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the running Tenant answered %s", resp.Status)
	}
	var body struct {
		Machines []struct {
			ID     string     `json:"id"`
			Update *hubUpdate `json:"update"`
		} `json:"machines"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body); err != nil {
		return nil, err
	}
	for _, m := range body.Machines {
		if m.ID == id {
			return m.Update, nil
		}
	}
	return nil, nil
}
