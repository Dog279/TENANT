package main

// childupdate.go (TEN-533; docs/plans/gap6-fleet-plan.md, section 8): a fleet
// child replaced by a newer Tenant its master sent over the link, with no way
// to the internet of its own and no moment at which the machine has no
// working child.
//
// How it goes, and who does what:
//
//  0. Whose word it is taken on (decision A71, child.json "updates"). By
//     default the release's own: the master's first piece of each send, the
//     one with no data, carries the release's checksums.txt and signature
//     bundle (verifyRelease), and nothing else is taken until they verify on
//     this machine against the release identity built into its binary. With
//     "updates": "master", the SHA-256 the master names, as before.
//  1. The master sends the archive in pieces with its SHA-256 (fleet/update).
//     The child keeps them in <dir>-data/update/<sha256>.part and touches
//     nothing else until the last piece is in and the whole file hashes to
//     what the master said. Then it unpacks the binary for this OS beside the
//     installed one (same filesystem, for the swap), and runs it with
//     `version`: it must run here and say the version it was sent as. Only
//     then does the master hear "applying". Any failure up to here deletes
//     what arrived and changes nothing.
//  2. The trial. The running child lets the calls it is running finish
//     (childDrain; its master sends it no new one from "applying" on, and
//     keeps them back until this machine says how the update went:
//     fleet.Manager, TEN-552). Then it drops its link, ends its background
//     commands and its browser, gives up its audit log, and starts the new
//     binary from where it was unpacked, as `child run --trial`, reading its
//     standard input: the trial ends when the old child closes it or dies.
//     The old child waits, up to childUpdateHealth, for the trial to say hello
//     to the master (child-update.json goes to StepProven). If the trial
//     exits, or the time passes, the old child stops it, deletes it, records
//     why, and takes its link back on the old version: rolled back.
//  3. The swap. The trial proved the new binary starts and reaches the
//     master on this machine. The old child stops it, records StepCommit with
//     a deadline, and swaps the binaries the way `tenant update` does
//     (selfupdate.Swap), keeping the old one beside it.
//  4. The restart, by the child itself, not by the service manager (whose
//     restart rules differ by OS and by what an operator installed): on
//     Linux and macOS the process execs the new binary in place, same
//     process, same unit; on Windows, which has no exec, the process stays as
//     a keeper of the new one (childKeep), passing a stop on to it and
//     starting it again when it asks.
//  5. The new child reads StepCommit. Its first hello to the master marks the
//     update done. If the deadline passes first, it puts the old binary back
//     itself and restarts into it: rolled back.
//
// A process that starts on the directory and finds an update in progress
// that is not its own to confirm (the old child was killed during the trial,
// or between the steps) finishes it as rolled back: the installed binary is
// the old one until the swap, and the swap is the last thing before the
// restart.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/fsperm"
	"tenant/internal/procprotect"
	"tenant/internal/selfupdate"
	"tenant/internal/version"
)

// childRestartCode is the exit code with which a child kept by an older
// process (Windows, childKeep) asks to be started again: after its own
// update, or after it put the old binary back. EX_TEMPFAIL.
const childRestartCode = 75

// errNotReplaced: the new version could not be started in this process's
// place, and this process is still the old one.
var errNotReplaced = errors.New("the new version could not be started")

var (
	// childUpdateHealth is how long the new binary has to reach the master,
	// in its trial and again once it is installed. Tests shorten it.
	childUpdateHealth = 60 * time.Second
	// childUpdateDrain is how long a child with an update to apply gives the
	// calls it is running to finish before it drops its link (childDrain).
	// With childUpdateHealth it has to fit inside fleet.UpdateHold, the time
	// the master keeps new calls back for. Tests shorten it.
	childUpdateDrain = 2 * time.Minute
	// childCommand starts this binary, or the one an update unpacked, again:
	// the smoke test, the trial, the restart. With no console window: a child
	// runs as a service with no console, and Windows would open one for a
	// console program it starts. Tests replace it with a helper process.
	childCommand = func(ctx context.Context, exe string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, exe, args...)
		childNoWindow(cmd)
		return cmd
	}
)

// childStageDir is where an update's binary is unpacked: beside the
// installed one, so the swap is a rename on one filesystem.
func childStageDir(exe string) string {
	return filepath.Join(filepath.Dir(exe), ".tenant-child-update")
}

// childStaged is an update whose archive arrived whole and checked, and whose
// binary is unpacked and ran.
type childStaged struct {
	to, sha, binary string
}

// childUpdater takes the pieces of an update (fleet.ClientConfig.Update).
type childUpdater struct {
	dir, exe string
	log      *slog.Logger
	ready    chan childStaged // the staged update, for childRun to apply

	mu       sync.Mutex
	applying *childStaged
	verified string // the SHA-256 the release's verified material lists for this machine's archive
}

var (
	// childReleaseRepo is the release identity an update's signature is
	// matched against: the repository this binary was built from, stamped
	// in at release time (version.Repo). Never the master's word, never the
	// environment: a child that could be told whose signature to accept
	// would accept anyone's. Tests replace it.
	childReleaseRepo = func() string { return version.Repo }
	// childCosign runs cosign with args, offline, with no console window.
	// Tests replace it.
	childCosign = func(ctx context.Context, args ...string) error {
		path, err := exec.LookPath("cosign")
		if err != nil {
			return errors.New("cosign is not installed on this machine, and the release's signature is verified with it")
		}
		cmd := exec.CommandContext(ctx, path, args...)
		childNoWindow(cmd)
		if out, err := procprotect.CombinedOutput(cmd); err != nil {
			said, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
			return fmt.Errorf("%v: %s", err, fleetQuote(said))
		}
		return nil
	}
)

// errNoReleaseIdentity: this build names no release, so the default rule
// can verify nothing. Said at install, in status, and to the master.
const errNoReleaseIdentity = "this build of Tenant names no release to verify an update against (a development build), and this machine takes only updates whose release signature it verified: it refuses every update until `tenant child updates master` makes it trust its master's hash"

// childReason keeps what the master is told one bounded sentence.
func childReason(err error) error {
	s := err.Error()
	if len(s) > 1024 {
		s = strings.ToValidUTF8(s[:1024], "") + "..."
	}
	return errors.New(s)
}

// verifyRelease is the default rule (decisions A71, TEN-542), on this
// machine and with no network: the archive named is this machine's for the
// version named; the checksums are signed, either with the project key this
// binary carries when the master sent a project signature (and then that
// signature alone decides), or else by the release workflow of the
// repository this binary was built from (cosign); and the verified
// checksums list the archive with the SHA-256 the master sent. The archive
// itself is then checked against that hash when it is whole. It says which
// signature verified (fleet.VerifiedProjectKey or fleet.VerifiedCosign).
func (u *childUpdater) verifyRelease(ctx context.Context, p fleet.Update, dir string) (string, error) {
	repo := childReleaseRepo()
	keys, err := selfupdate.ParseKeys(version.ReleaseKeys)
	if err != nil {
		return "", fmt.Errorf("this build's project key does not read (%v), and this machine takes no update until it runs a build whose key does", err)
	}
	keyed := len(keys) > 0 && len(p.Signature) > 0
	switch want := selfupdate.AssetName(p.Version, runtime.GOOS, runtime.GOARCH); {
	case repo == "" && len(keys) == 0:
		return "", errors.New(errNoReleaseIdentity)
	case p.Asset == "" || len(p.Checksums) == 0:
		return "", errors.New("the master sent no release checksums or signature with the update, and this machine takes only an update whose release signature it verified " +
			"(update the master's Tenant, or on this machine `tenant child updates master`)")
	case p.Asset != want:
		return "", fmt.Errorf("the archive is %s, not %s, this machine's for version %s", fleetQuote(p.Asset), want, fleetQuote(p.Version))
	case keyed:
		if _, err := selfupdate.VerifyProject(p.Checksums, p.Signature, keys); err != nil {
			return "", fmt.Errorf("the release's project signature does not verify on this machine: %v", err)
		}
		if err := checksumsList(p); err != nil {
			return "", err
		}
		return fleet.VerifiedProjectKey, nil
	case len(p.Bundle) == 0 && len(keys) > 0:
		return "", errors.New("the master sent neither a project signature nor a signature bundle with the release, and this machine takes only an update whose release signature it verified " +
			"(`tenant child updates master` on this machine trusts the master's hash instead)")
	case len(p.Bundle) == 0:
		return "", errors.New("the release carries no signature bundle, and this machine takes only an update whose release signature it verified " +
			"(`tenant child updates master` on this machine trusts the master's hash instead)")
	case repo == "":
		return "", errors.New("the master sent no project signature with the release, and this build names no release for cosign to verify its signature bundle against")
	}
	tmp, err := os.MkdirTemp(dir, ".release-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	sums, bundle := filepath.Join(tmp, "checksums.txt"), filepath.Join(tmp, "checksums.txt.sigstore.json")
	if err := os.WriteFile(sums, p.Checksums, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(bundle, p.Bundle, 0o600); err != nil {
		return "", err
	}
	vctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := childCosign(vctx, selfupdate.CosignArgs(sums, bundle, repo, true)...); err != nil {
		if len(keys) > 0 {
			return "", fmt.Errorf("the master sent no project signature with the release (its Tenant may be older, or the release carries none), so this machine verified the release workflow's signature with cosign instead, and it does not verify as %s's release workflow: %v", repo, err)
		}
		return "", fmt.Errorf("the release's signature does not verify as %s's release workflow on this machine: %v", repo, err)
	}
	if err := checksumsList(p); err != nil {
		return "", err
	}
	return fleet.VerifiedCosign, nil
}

// checksumsList checks that a release's verified checksums list the archive
// with the SHA-256 the master named.
func checksumsList(p fleet.Update) error {
	switch got, ok := selfupdate.ParseChecksums(bytes.NewReader(p.Checksums))[p.Asset]; {
	case !ok:
		return fmt.Errorf("the release's checksums list no %s", p.Asset)
	case got != p.SHA256:
		return fmt.Errorf("the release's checksums list another SHA-256 for %s than the master named", p.Asset)
	}
	return nil
}

func newChildUpdater(dir, exe string, log *slog.Logger) *childUpdater {
	return &childUpdater{dir: dir, exe: exe, log: log, ready: make(chan childStaged, 1)}
}

// reset forgets an update that was rolled back, so another can be sent.
func (u *childUpdater) reset() {
	u.mu.Lock()
	u.applying = nil
	u.mu.Unlock()
}

// piece takes one piece of an archive. The master's pieces come in order; a
// piece at another offset, or one with no data, is answered with how much is
// here, and the master goes on from there. What arrives is kept on disk under
// its SHA-256, so a link that drops mid-transfer goes on where it stopped.
func (u *childUpdater) piece(ctx context.Context, p fleet.Update) (fleet.UpdateAck, error) {
	ack, err := u.take(ctx, p)
	if err != nil {
		return fleet.UpdateAck{}, childReason(err)
	}
	return ack, nil
}

func (u *childUpdater) take(ctx context.Context, p fleet.Update) (fleet.UpdateAck, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	// The rule is child.json's as it is now: the operator's word on this
	// machine, read at every piece.
	child, err := fleet.LoadChild(fleet.ChildPath(u.dir))
	if err != nil {
		return fleet.UpdateAck{}, err
	}
	rule := child.Rule()
	if u.applying != nil {
		if u.applying.sha == p.SHA256 {
			return fleet.UpdateAck{Received: p.Size, Applying: true, Rule: string(rule)}, nil
		}
		return fleet.UpdateAck{}, errors.New("this machine is already applying another update")
	}
	dir := filepath.Join(childDataDir(u.dir), "update")
	if err := fsperm.MkdirAll(dir); err != nil {
		return fleet.UpdateAck{}, err
	}
	// One transfer at a time: anything else in the folder is an older one.
	part := filepath.Join(dir, p.SHA256+".part") // p.SHA256 is 64 lowercase hex digits (fleet.Update.validate)
	how := ""
	if rule == fleet.UpdatesRelease {
		sent := p.Asset != "" || len(p.Checksums) > 0 || len(p.Bundle) > 0 || len(p.Signature) > 0
		if sent || u.verified != p.SHA256 {
			u.verified = ""
			var err error
			if how, err = u.verifyRelease(ctx, p, dir); err != nil {
				_ = os.Remove(part) // nothing of an archive this machine will not take stays
				return fleet.UpdateAck{}, fmt.Errorf("%w; nothing was changed", err)
			}
			u.verified = p.SHA256
		}
	}
	ack, err := u.write(ctx, p, dir, part)
	ack.Rule, ack.Verified = string(rule), how
	return ack, err
}

// write takes one piece of an archive whose rule has been applied.
func (u *childUpdater) write(ctx context.Context, p fleet.Update, dir, part string) (fleet.UpdateAck, error) {
	if old, err := os.ReadDir(dir); err == nil {
		for _, e := range old {
			if e.Name() != filepath.Base(part) {
				_ = os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
	var have int64
	if fi, err := os.Stat(part); err == nil {
		have = fi.Size()
	}
	if have > p.Size {
		_ = os.Remove(part)
		have = 0
	}
	if len(p.Data) == 0 || p.Offset != have {
		return fleet.UpdateAck{Received: have}, nil
	}
	f, err := fsperm.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return fleet.UpdateAck{}, err
	}
	_, err = f.Write(p.Data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(part) // a torn piece: start again
		return fleet.UpdateAck{}, err
	}
	if have += int64(len(p.Data)); have < p.Size {
		return fleet.UpdateAck{Received: have}, nil
	}
	// Whole. Untrusted until its hash is the one it was sent with.
	defer os.Remove(part)
	got, err := selfupdate.SHA256(part)
	if err != nil {
		return fleet.UpdateAck{}, err
	}
	if got != p.SHA256 {
		u.log.Warn("fleet child: an update arrived damaged and was deleted", "want", p.SHA256, "got", got)
		return fleet.UpdateAck{}, errors.New("the archive that arrived does not hash to the SHA-256 it was sent with: it was deleted and nothing was changed")
	}
	bin, err := u.stage(ctx, part, p.Version)
	if err != nil {
		_ = os.RemoveAll(childStageDir(u.exe))
		return fleet.UpdateAck{}, fmt.Errorf("%w; nothing was changed", err)
	}
	s := childStaged{to: p.Version, sha: p.SHA256, binary: bin}
	u.applying = &s
	select {
	case u.ready <- s:
	default:
	}
	u.log.Info("fleet child: an update arrived whole and checked; applying it", "to", p.Version)
	return fleet.UpdateAck{Received: p.Size, Applying: true}, nil
}

// stage unpacks this OS's binary from the archive beside the installed one
// and runs it once: it must run here, and say it is version want.
func (u *childUpdater) stage(ctx context.Context, archive, want string) (string, error) {
	ext, err := archiveKind(archive)
	if err != nil {
		return "", err
	}
	named := archive + ext // ExtractBinary goes by the name
	if err := os.Rename(archive, named); err != nil {
		return "", err
	}
	defer os.Remove(named)
	stage := childStageDir(u.exe)
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return "", fmt.Errorf("the folder of this machine's tenant binary cannot be written to: %w", err)
	}
	bin := filepath.Join(stage, exeName())
	if err := selfupdate.ExtractBinary(named, bin, exeName()); err != nil {
		return "", fmt.Errorf("the archive is not a Tenant for this machine (%s/%s): %w", runtime.GOOS, runtime.GOARCH, err)
	}
	// macOS: as `tenant update` does (codesign.go), before it first runs.
	if signsBinaries {
		if err := signHardened(ctx, bin); err != nil {
			return "", fmt.Errorf("the new binary could not be signed with the hardened runtime: %w", err)
		}
	}
	smoke, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := procprotect.Output(childCommand(smoke, bin, "version"))
	if err != nil {
		return "", fmt.Errorf("the new binary does not run on this machine (%s/%s): %v", runtime.GOOS, runtime.GOARCH, err)
	}
	if f := strings.Fields(string(out)); len(f) < 2 || f[1] != want {
		said, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return "", fmt.Errorf("the new binary says it is %s, not version %s", fleetQuote(said), want)
	}
	return bin, nil
}

// archiveKind tells a zip from a gzip'd tar by their first bytes.
func archiveKind(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var head [4]byte
	if _, err := io.ReadFull(f, head[:]); err == nil {
		switch {
		case string(head[:]) == "PK\x03\x04":
			return ".zip", nil
		case head[0] == 0x1f && head[1] == 0x8b:
			return ".tar.gz", nil
		}
	}
	return "", errors.New("the archive is neither a zip nor a gzip'd tar")
}

// --- the update, step by step ---

func childUpdateNow() time.Time { return time.Now().UTC().Truncate(time.Second) }

// childDrain is step 2's first half (TEN-552): before the child drops its
// link for an update, the calls it is running get to finish, up to
// childUpdateDrain. A call cut off by the restart is one whose outcome its
// master cannot know. The master sends no new call meanwhile (it keeps them
// back from the moment this machine said "applying"), so the count only
// falls; an older master still sends them, and then the limit is what ends
// the wait. The calls still running at the limit end with the link, as every
// running call did before.
func childDrain(ctx context.Context, running func() int, log *slog.Logger) {
	n := running()
	if n == 0 {
		return
	}
	log.Info("fleet child: an update is ready; the calls that are running finish first", "calls", n, "limit", childUpdateDrain)
	for deadline := time.Now().Add(childUpdateDrain); n > 0 && time.Now().Before(deadline) && sleepCtx(ctx, 100*time.Millisecond); {
		n = running()
	}
	if n > 0 && ctx.Err() == nil {
		log.Warn("fleet child: calls are still running after the time an update gives them; they end with the restart", "calls", n, "limit", childUpdateDrain)
	}
}

// rolledBack records an update as rolled back, and why.
func rolledBack(dir string, st fleet.ChildUpdate, reason string, log *slog.Logger) {
	st.State, st.Step, st.Reason, st.At, st.Deadline = fleet.UpdateRolledBack, "", reason, childUpdateNow(), time.Time{}
	if err := fleet.WriteChildUpdate(dir, st); err != nil {
		log.Warn("fleet child: could not record the rolled-back update", "err", err)
	}
	log.Warn("fleet child: update rolled back; this machine runs the version it had", "from", st.From, "to", st.To, "why", reason)
}

// childApply is steps 2 and 3: the trial run of the staged binary, and its
// swap into place once it has reached the master. restart is true when the
// new binary is in place and this process must give way to it; false when
// the update was rolled back and this process carries on with the old one.
// The link, the tools and the audit log are already given up.
func childApply(ctx context.Context, dir, exe string, s childStaged, log *slog.Logger) (restart bool) {
	st := fleet.ChildUpdate{UpdateReport: fleet.UpdateReport{From: version.Version, To: s.to, State: fleet.UpdateTrial, At: childUpdateNow()},
		Step: fleet.StepTrial, SHA256: s.sha}
	if err := fleet.WriteChildUpdate(dir, st); err != nil {
		_ = os.RemoveAll(childStageDir(exe))
		log.Warn("fleet child: the update was not applied: its state cannot be recorded", "err", err)
		return false
	}
	fail := func(reason string) bool {
		_ = os.RemoveAll(childStageDir(exe))
		rolledBack(dir, st, reason, log)
		return false
	}
	trial, err := startChildProc(s.binary, dir, "--trial")
	if err != nil {
		return fail("the new version could not be started: " + err.Error())
	}
	proven := false
	deadline := time.NewTimer(childUpdateHealth)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for !proven {
		select {
		case <-ctx.Done():
			trial.stop()
			return fail("the child was stopped during the trial run of the new version")
		case <-trial.done:
			return fail("the new version stopped before it reached the master: " + trial.result())
		case <-deadline.C:
			trial.stop()
			return fail(fmt.Sprintf("the new version did not reach the master within %s", childUpdateHealth))
		case <-tick.C:
			cur, ok, _ := fleet.ReadChildUpdate(dir)
			proven = ok && cur.Step == fleet.StepProven && cur.To == s.to
		}
	}
	trial.stop()
	st.Step, st.Deadline = fleet.StepCommit, childUpdateNow().Add(2*childUpdateHealth)
	if err := fleet.WriteChildUpdate(dir, st); err != nil {
		return fail("the update's state cannot be recorded: " + err.Error())
	}
	if err := childSwap(exe, s.binary); err != nil {
		return fail("the new version could not be put in place: " + err.Error())
	}
	_ = os.RemoveAll(childStageDir(exe))
	log.Info("fleet child: the new version reached the master in its trial and is in place; restarting into it", "to", s.to)
	return true
}

// childProc is a child process of a child: a trial, or (Windows) the child a
// keeper keeps. It reads its standard input and ends when that closes.
type childProc struct {
	cmd   *exec.Cmd
	in    io.Closer
	kill  context.CancelFunc
	done  chan struct{}
	err   error
	once  sync.Once
	until time.Duration
	said  *tailBuffer // the end of what it wrote to standard error
}

// tailBuffer keeps the last bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - 4096; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

// last is the last line written that is not empty, cut short.
func (t *tailBuffer) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if len(line) > 200 {
		line = line[:200] + "..."
	}
	return line
}

func startChildProc(exe, dir string, mode string) (*childProc, error) {
	ctx, kill := context.WithCancel(context.Background())
	cmd := childCommand(ctx, exe, "child", "run", "--dir", dir, mode)
	said := &tailBuffer{}
	cmd.Stderr = said
	var in io.WriteCloser
	var err error
	procprotect.Child(cmd)
	procprotect.Ordinary(func() { // the pipes, its input and its standard error, are made here
		if in, err = cmd.StdinPipe(); err == nil {
			err = cmd.Start()
		}
	})
	if err != nil {
		kill()
		return nil, err
	}
	p := &childProc{cmd: cmd, in: in, kill: kill, done: make(chan struct{}), until: 30 * time.Second, said: said}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// stop asks the process to end (its standard input closes), and kills it if
// it has not within until.
func (p *childProc) stop() {
	p.once.Do(func() {
		_ = p.in.Close()
		select {
		case <-p.done:
		case <-time.After(p.until):
			p.kill()
			<-p.done
		}
		p.kill()
	})
}

// result is how the process ended, in words, with the last line it wrote
// to standard error. Call it after done.
func (p *childProc) result() string {
	out := "it exited with code 0"
	var exit *exec.ExitError
	if errors.As(p.err, &exit) {
		out = fmt.Sprintf("it exited with code %d", exit.ExitCode())
	} else if p.err != nil {
		out = p.err.Error()
	}
	if line := p.said.last(); line != "" {
		out += " (" + line + ")"
	}
	return out
}

// code is the process's exit code, -1 when it did not exit by itself.
func (p *childProc) code() int {
	var exit *exec.ExitError
	switch {
	case p.err == nil:
		return 0
	case errors.As(p.err, &exit):
		return exit.ExitCode()
	}
	return -1
}

// childSwap is selfupdate.Swap, with room made for it: on Windows the
// previous binary may still run (a keeper, childKeep), and a running binary
// cannot be deleted but can be renamed, so it is renamed aside to be
// deleted once it has exited.
func childSwap(exe, staged string) error {
	prev := selfupdate.PrevPath(exe)
	if runtime.GOOS == "windows" {
		if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(prev, fmt.Sprintf("%s.%d.old", prev, time.Now().UnixNano())); err != nil {
				return err
			}
		}
	}
	return selfupdate.Swap(exe, staged)
}

// sweepOldBinaries deletes binaries childSwap renamed aside, those whose
// process has exited.
func sweepOldBinaries(exe string) {
	old, _ := filepath.Glob(selfupdate.PrevPath(exe) + ".*.old")
	for _, p := range old {
		_ = os.Remove(p)
	}
}

// --- what the next process does ---

// childPending is an update in progress as a starting child finds it.
type childPending struct {
	dir string
	log *slog.Logger

	mu        sync.Mutex
	st        fleet.ChildUpdate
	confirm   bool          // this process is the new version, and must reach the master
	confirmed chan struct{} // closed by the hello that does
	expired   bool          // the deadline passed first: no hello confirms it any more
}

// settleChildUpdate finishes what the last process on dir left of an update.
// It returns the update this process has to confirm, if it is the new
// version on its first run; otherwise it records an unfinished update as
// rolled back, since the installed binary is the old one until StepCommit.
func settleChildUpdate(dir, exe string, log *slog.Logger) *childPending {
	sweepOldBinaries(exe)
	st, ok, err := fleet.ReadChildUpdate(dir)
	if err != nil {
		log.Warn("fleet child: the record of the last update cannot be read", "err", err)
	}
	if !ok || st.State != fleet.UpdateTrial {
		return &childPending{dir: dir, log: log, st: st, confirmed: make(chan struct{})}
	}
	p := &childPending{dir: dir, log: log, st: st, confirmed: make(chan struct{})}
	switch {
	case st.Step == fleet.StepCommit && st.To == version.Version:
		p.confirm = true
	case st.Step == fleet.StepCommit:
		rolledBack(dir, st, "the update was interrupted before the new version first ran; this machine kept the version it had", log)
		p.st, _, _ = fleet.ReadChildUpdate(dir)
	default:
		_ = os.RemoveAll(childStageDir(exe))
		rolledBack(dir, st, "the update was interrupted during the trial run of the new version; this machine kept the version it had", log)
		p.st, _, _ = fleet.ReadChildUpdate(dir)
	}
	return p
}

// hello is fleet.ClientConfig.Hello: what this child says of the last
// update, and, for the new version on its first run, the moment the update
// is done: the master is saying hello.
func (p *childPending) hello() *fleet.UpdateReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.confirm && !p.expired {
		p.confirm = false
		p.st.State, p.st.Step, p.st.Reason, p.st.At, p.st.Deadline = fleet.UpdateDone, "", "", childUpdateNow(), time.Time{}
		if err := fleet.WriteChildUpdate(p.dir, p.st); err != nil {
			p.log.Warn("fleet child: could not record the finished update", "err", err)
		}
		p.log.Info("fleet child: updated; the new version reached the master", "from", p.st.From, "to", p.st.To)
		close(p.confirmed)
	}
	// The file as it is now: an update this process applied and rolled back
	// since it started is told too.
	st, ok, _ := fleet.ReadChildUpdate(p.dir)
	if !ok {
		return nil
	}
	return reportOf(st)
}

// trialHello is the trial's Hello: the hello is the proof the old child is
// waiting for.
func trialHello(dir string) func() *fleet.UpdateReport {
	return func() *fleet.UpdateReport {
		st, ok, _ := fleet.ReadChildUpdate(dir)
		if !ok {
			return nil
		}
		if st.State == fleet.UpdateTrial && st.Step == fleet.StepTrial && st.To == version.Version {
			st.Step = fleet.StepProven
			_ = fleet.WriteChildUpdate(dir, st)
		}
		return reportOf(st)
	}
}

func reportOf(st fleet.ChildUpdate) *fleet.UpdateReport {
	if st.To == "" {
		return nil
	}
	r := st.UpdateReport
	return &r
}

// expire is the deadline of an update this process must confirm: false when
// the hello came first; true when it did not, and then no hello confirms it.
func (p *childPending) expire() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.confirm {
		return false
	}
	p.expired = true
	return true
}

// rollBack is the new version giving up its place: the old binary goes back
// where it was, and the update is recorded as rolled back.
func (p *childPending) rollBack(exe string) error {
	if err := selfupdate.Rollback(exe); err != nil {
		return err
	}
	p.mu.Lock()
	st := p.st
	p.mu.Unlock()
	rolledBack(p.dir, st, fmt.Sprintf("the new version ran but did not reach the master by %s", st.Deadline.Local().Format(time.DateTime)), p.log)
	return nil
}
