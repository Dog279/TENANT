package main

// childctl.go (TEN-531): `tenant child`, the side of a fleet that runs on a
// machine its operator owns. A child is this same binary with no model, no
// agent loop, no dashboard and no memory: it holds one link to its master
// (internal/fleet) and serves this machine's tools over it (childmux.go).
//
//	tenant child install [--system] [--ceiling C] [--sandbox B]
//	tenant child status | pause | resume | audit | uninstall
//	tenant child run            what the installed service runs
//
// Exit codes: 0 done; 1 an error; 2 refused before anything was changed
// (the enrollment code given as an argument, a machine that is already
// enrolled, --system without elevation); 3 from status, when the machine is
// not connected to its master.
//
// A child keeps everything of its own in one directory: this account's
// (<config dir>/child), or with --system the machine's (fleet.SystemDir).
// What its tools work with goes beside it, in <dir>-data.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"tenant/internal/fleet"
	"tenant/internal/fleet/shadow"
	"tenant/internal/fsperm"
	"tenant/internal/memory/soul"
	"tenant/internal/sandbox"
	"tenant/internal/selfupdate"
	"tenant/internal/version"
)

const childUsage = "usage: tenant child install [--system] [--ceiling full|no-exec|read-only] | status | pause | resume | audit | uninstall"

// childCodeEnv is the environment variable `tenant child install` reads the
// enrollment code from when it is set, before standard input.
const childCodeEnv = "TENANT_ENROLL_CODE"

// Seams. Tests keep a child away from this machine's own system directory
// and its own system log, and shorten the watch.
var (
	childSystemDir       = fleet.SystemDir
	openChildOSLog       = fleet.OpenOSLog
	registerChildOSLog   = fleet.RegisterOSLog
	unregisterChildOSLog = fleet.UnregisterOSLog
	checkChildExe        = childExeSafe
	// childPoll is how often a running child looks for the pause mark.
	childPoll = time.Second
)

func cmdChild(ctx context.Context, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(childUsage)
	}
	verb, args := args[0], args[1:]
	fs := flag.NewFlagSet("child "+verb, flag.ContinueOnError)
	dirFlag := fs.String("dir", "", "the child's directory (default: this account's, or with --system the machine's)")
	system := fs.Bool("system", false, "install: run as root or SYSTEM from boot, with the child's files in the machine's own location (needs an elevated terminal)")
	ceiling := fs.String("ceiling", string(fleet.CeilingFull), "install: what this machine lets its master do: full, no-exec or read-only")
	sandboxName := fs.String("sandbox", "host", "install: the sandbox backend this machine runs commands through")
	hidden := fs.Bool("hidden", false, "run: Windows: hide the console window (the logon task passes it)")
	// An update's own (childupdate.go): the new version's trial run, and on
	// Windows the child an older process keeps. Both end when their standard
	// input closes.
	trial := fs.Bool("trial", false, "run: the trial run of an update (started by the child itself)")
	kept := fs.Bool("kept", false, "run: kept by an older child process after an update (started by the child itself)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		if verb == "install" {
			return exitError{2, "the enrollment code is read from standard input or from $" + childCodeEnv +
				", never from an argument: an argument shows in the process list and stays in the shell's history"}
		}
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), childUsage)
	}
	if *system && *dirFlag != "" {
		return exitError{2, "--system keeps the child's files in " + childSystemDir() + "; it does not take --dir"}
	}
	dir, err := childDir(verb, *dirFlag, *system)
	if err != nil {
		return err
	}
	switch verb {
	case "install":
		return childInstall(ctx, os.Stdin, os.Stdout, dir, childInstallOpts{system: *system, ceiling: *ceiling, sandbox: *sandboxName})
	case "run":
		if *hidden {
			hideConsoleWindow()
		}
		return childRun(ctx, dir, newLogger(), childRunOpts{trial: *trial, kept: *kept})
	case "status":
		return childStatus(ctx, os.Stdout, dir)
	case "pause", "resume":
		return childPause(os.Stdout, dir, verb == "pause")
	case "audit":
		return childAudit(os.Stdout, dir)
	case "uninstall":
		return childUninstall(ctx, os.Stdout, dir)
	}
	return fmt.Errorf("unknown child command %q\n%s", verb, childUsage)
}

// childDir is the directory a command acts on: the one named, the machine's
// with --system, else this account's. Without either flag a command other
// than install acts on the machine's child when this account has none of
// its own, as `tenant service status` finds a --system service.
func childDir(verb, named string, system bool) (string, error) {
	switch {
	case named != "":
		return filepath.Abs(expandPath(named))
	case system:
		return childSystemDir(), nil
	}
	cfg, err := soul.DefaultDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	own := filepath.Join(cfg, "child")
	if verb != "install" && !exists(fleet.ChildPath(own)) && exists(fleet.ChildPath(childSystemDir())) {
		return childSystemDir(), nil
	}
	return own, nil
}

// exists reports that something is at path, or may be: a path this account
// may not look at counts, so that the command goes on to say so.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// isSystemChild reports whether dir is the machine's own child directory.
func isSystemChild(dir string) bool {
	a, b := filepath.Clean(dir), filepath.Clean(childSystemDir())
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// --- install ---

type childInstallOpts struct {
	system           bool
	ceiling, sandbox string
}

// childInstall enrolls this machine and installs the service that holds its
// link. Everything that can refuse does so before the code is spent, and the
// key, the certificate and child.json are in place, readable by the
// service's account alone, before the service is first started.
func childInstall(ctx context.Context, in io.Reader, out io.Writer, dir string, o childInstallOpts) error {
	switch fleet.Ceiling(o.ceiling) {
	case fleet.CeilingFull, fleet.CeilingNoExec, fleet.CeilingReadOnly:
	default:
		return exitError{2, fmt.Sprintf("--ceiling %q: it is full, no-exec or read-only", o.ceiling)}
	}
	if _, err := sandbox.For(o.sandbox); err != nil {
		// Found out now, and not by the first command the master sends.
		return exitError{2, fmt.Sprintf("--sandbox %q: %v", o.sandbox, err)}
	}
	if o.system && !childElevated() {
		return exitError{2, "tenant child install --system needs an elevated terminal: the service it installs runs as " + childAccount(true) +
			", and its key is written where only that account reads it"}
	}
	if exists(fleet.ChildPath(dir)) || exists(fleet.ChildIdentityPath(dir)) {
		return exitError{2, "this machine is already enrolled (" + dir + "): `tenant child status` shows where it stands. " +
			"To enroll it again, `tenant child uninstall` first: a new enrollment is a new key"}
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = resolveExe(exe)
	}
	if err != nil {
		return err
	}
	if o.system {
		if err := checkChildExe(exe); err != nil {
			return exitError{2, err.Error()}
		}
	}
	spec, err := childSpecFor(runtime.GOOS, exe, dir, o.system)
	if err != nil {
		return err
	}
	code, err := readEnrollCode(in, out)
	if err != nil {
		return err
	}

	id, err := fleet.MintIdentity(fleet.ChildIdentityPath(dir))
	if err != nil {
		return err
	}
	child, digits, err := fleet.Enroll(ctx, code, id, fleet.Reported{OS: runtime.GOOS, User: childAccount(o.system),
		Version: version.Version, Sandbox: o.sandbox, Ceiling: o.ceiling})
	if err == nil {
		err = fleet.SaveChild(fleet.ChildPath(dir), child)
	}
	if err == nil {
		err = fsperm.MkdirAll(childDataDir(dir))
	}
	if err == nil && o.system {
		if err = sealSystemChild(dir, childDataDir(dir)); err != nil {
			err = fmt.Errorf("the child's files could not be closed to other accounts, so the service was not installed: %w", err)
		}
	}
	if err != nil {
		// A key nobody enrolled, or one whose files are not safe, is no use:
		// the next attempt starts clean. A code is spent once it was sent.
		for _, p := range []string{fleet.ChildPath(dir), fleet.ChildIdentityPath(dir), childDataDir(dir), dir} {
			_ = os.Remove(p) // the two directories only if nothing else is in them
		}
		return err
	}
	fmt.Fprintf(out, "Enrolled as %s (%s). This machine now waits for its master to confirm it.\n\n    confirmation code: %s\n\n"+
		"Type that code on the master (tenant fleet confirm %s). Until then the master sends this machine nothing.\n",
		child.Name, child.ID, digits, child.Name)
	if o.system {
		if err := registerChildOSLog(); err != nil {
			fmt.Fprintf(out, "note: %v (the audit lines still reach the system log, under a longer heading)\n", err)
		}
	}
	if err := spec.install(ctx); err != nil {
		return fmt.Errorf("this machine is enrolled, but its service was not installed: %w\n"+
			"`tenant child run --dir %q` holds the link from this terminal; `tenant child uninstall` undoes the enrollment", err, dir)
	}
	_, where := spec.installed(ctx)
	when := "every time this account logs on"
	if o.system {
		when = "at every boot"
	}
	fmt.Fprintf(out, "Installed %s: it connects again %s. `tenant child status` shows where this machine stands.\n", where, when)
	return nil
}

// readEnrollCode takes the one-time code from $TENANT_ENROLL_CODE or, when
// that is not set, from the first line of standard input. The variable is
// taken out of this process's environment at once, so nothing this command
// starts inherits it.
func readEnrollCode(in io.Reader, out io.Writer) (string, error) {
	code := strings.TrimSpace(os.Getenv(childCodeEnv))
	_ = os.Unsetenv(childCodeEnv)
	if code == "" {
		if f, ok := in.(*os.File); ok && isatty.IsTerminal(f.Fd()) {
			fmt.Fprint(out, "Enrollment code (from `tenant fleet enroll <name>` on the master): ")
		}
		sc := bufio.NewScanner(io.LimitReader(in, 64<<10))
		sc.Buffer(make([]byte, 0, 4096), 64<<10)
		for sc.Scan() && code == "" {
			code = strings.TrimSpace(sc.Text())
		}
	}
	if code == "" {
		return "", exitError{2, "no enrollment code: pipe it on standard input or set $" + childCodeEnv +
			" (`tenant fleet enroll <name>` on the master prints one)"}
	}
	return code, nil
}

// childAccount names the account a child runs as: what the master's confirm
// screen shows, marked as the machine's own word.
func childAccount(system bool) string {
	if system {
		if runtime.GOOS == "windows" {
			return "SYSTEM"
		}
		return "root"
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return firstNonEmpty(os.Getenv("USER"), os.Getenv("USERNAME"), "unknown")
}

// --- run ---

// childRunOpts are the two ways an update starts a child itself
// (childupdate.go). Both read standard input and stop when it closes.
type childRunOpts struct {
	trial bool // the new version's trial run: it applies no update and settles none
	kept  bool // kept by an older process (Windows): it restarts by exiting with childRestartCode
}

// childRun is what the installed service runs: it holds the link to the
// master, serving this machine's tools behind the gate, until it is stopped
// or the master revokes the machine. While the pause mark is there it holds
// no link; the mark is looked for once a second. An update the master sends
// is applied between two links (childApply), and this process then gives
// way to the new version (childReplace).
func childRun(ctx context.Context, dir string, log *slog.Logger, o childRunOpts) error {
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if o.trial || o.kept {
		var stop context.CancelFunc
		ctx, stop = context.WithCancel(ctx)
		defer stop()
		go func() { // the process that started this one closed its end, or died
			_, _ = io.Copy(io.Discard, os.Stdin)
			stop()
		}()
	}
	if !exists(fleet.ChildPath(dir)) {
		return fmt.Errorf("no child is enrolled in %s (tenant child install)", dir)
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = resolveExe(exe)
	}
	if err != nil {
		return err
	}
	var (
		pending = &childPending{dir: dir, log: log, confirmed: make(chan struct{})}
		hello   = trialHello(dir)
		upd     *childUpdater
	)
	if !o.trial {
		pending = settleChildUpdate(dir, exe, log)
		hello, upd = pending.hello, newChildUpdater(dir, exe, log)
	}
	for {
		serveCtx, stopServing := context.WithCancel(ctx)
		expired := make(chan struct{})
		if pending.confirm {
			// The new version on its first run: the master's hello by the
			// deadline, or the old version back.
			go func() {
				t := time.NewTimer(max(0, time.Until(pending.st.Deadline)))
				defer t.Stop()
				select {
				case <-t.C:
					if pending.expire() {
						close(expired)
						stopServing()
					}
				case <-pending.confirmed:
				case <-serveCtx.Done():
				}
			}()
		}
		staged, err := childServe(serveCtx, dir, log, hello, upd)
		stopServing()
		select {
		case <-expired:
			if err := pending.rollBack(exe); err != nil {
				log.Error("fleet child: the new version did not reach the master, and the old one could not be put back", "err", err)
				return err
			}
			return childReplace(ctx, exe, dir, o.kept, log)
		default:
		}
		if staged == nil || ctx.Err() != nil {
			return err
		}
		if !childApply(ctx, dir, exe, *staged, log) {
			upd.reset()
			continue // rolled back: the link again, on this version
		}
		err = childReplace(ctx, exe, dir, o.kept, log)
		if !errors.Is(err, errNotReplaced) {
			return err
		}
		// Not replaced, and still this process: the old binary back, and on.
		st, _, _ := fleet.ReadChildUpdate(dir)
		if rerr := selfupdate.Rollback(exe); rerr != nil {
			return fmt.Errorf("%w; and the old binary could not be put back: %v", err, rerr)
		}
		rolledBack(dir, st, "the new version could not be started in this process's place: "+err.Error(), log)
		upd.reset()
	}
}

// childServe holds the link, with this machine's tools behind the gate and
// its audit log open, until ctx ends, the master revokes the machine, or an
// update arrives whole and checked: then it gives up the link, the tools and
// the audit log, and returns the update for childRun to apply. upd is nil in
// a trial, which takes no update.
func childServe(ctx context.Context, dir string, log *slog.Logger, hello func() *fleet.UpdateReport, upd *childUpdater) (*childStaged, error) {
	var oslog func(string)
	if write, done, err := openChildOSLog(); err != nil {
		log.Warn("fleet child: the system log is not available; the audit log is kept in the child's own file only", "err", err)
	} else {
		defer done()
		var once sync.Once
		oslog = func(line string) {
			if err := write(line); err != nil {
				once.Do(func() {
					log.Warn("fleet child: an audit line did not reach the system log; the child's own file has it", "err", err)
				})
			}
		}
	}
	audit, err := fleet.OpenAudit(dir, oslog)
	if err != nil {
		return nil, err // another child runs on this directory
	}
	defer audit.Close()
	if err := audit.Ready(); err != nil {
		log.Error("fleet child: the audit log cannot be written, so every call is refused until it can", "err", err)
	}

	// The unit's working directory. An operator who cleared it out gets it
	// back, not a service that cannot start.
	if err := fsperm.MkdirAll(childDataDir(dir)); err != nil {
		log.Warn("fleet child: the working directory could not be made", "dir", childDataDir(dir), "err", err)
	}
	gate := fleet.NewGate(dir, audit, childRunsCommand)
	mux, stopTools := buildChildMux(ctx, dir, gate.Confirm, gate.Current, log)
	defer mux.Close()
	who := childAccount(false)
	// The copies of this machine's memory the master sends (TEN-532): sealed
	// snapshots it cannot read, kept inside the child's own directory, which
	// the file tools refuse. Without them the child serves its tools and says
	// it keeps no copy.
	memory, err := shadow.OpenChildStore(childMemoryDir(dir))
	if err != nil {
		log.Warn("fleet child: no copy of this machine's memory can be kept", "err", err)
		memory = nil
	}
	cfg := fleet.ClientConfig{Dir: dir, Tools: gate.Tools(mux), Version: version.Version, User: who, Hello: hello, Memory: memory, Log: log}
	var ready chan childStaged
	if upd != nil {
		cfg.Update, ready = upd.piece, upd.ready
	}

	var told string
	for {
		for fleet.Paused(dir) {
			if told != "paused" {
				told = "paused"
				log.Info("fleet child: paused on this machine; holding no link until `tenant child resume`")
			}
			if !sleepCtx(ctx, childPoll) {
				return nil, nil
			}
		}
		client, err := fleet.NewClient(cfg)
		if err != nil {
			// A child.json that does not load, or a key that is gone. The child
			// stays up and serves nothing, and takes the file up again when the
			// operator has put it right.
			if told != err.Error() {
				told = err.Error()
				log.Error("fleet child: no link can be held, and nothing is served, until this is put right", "err", err)
			}
			if !sleepCtx(ctx, 5*childPoll) {
				return nil, nil
			}
			continue
		}
		told = ""
		linkCtx, drop := context.WithCancel(ctx)
		go func() { // the pause, noticed while the link is up
			for sleepCtx(linkCtx, childPoll) {
				if fleet.Paused(dir) {
					drop()
				}
			}
		}()
		var staged *childStaged
		got := make(chan childStaged, 1)
		go func() { // an update, whole and checked: the link goes once the master has its answer
			select {
			case s := <-ready: // nil in a trial: never
				got <- s
				sleepCtx(linkCtx, time.Second)
				drop()
			case <-linkCtx.Done():
			}
		}()
		err = client.Run(linkCtx) // returns once the calls that were running have stopped
		drop()
		select {
		case s := <-got:
			staged = &s
		default:
		}
		switch {
		case staged != nil:
			stopTools() // the background commands and the browser end at the restart
			return staged, nil
		case ctx.Err() != nil:
			return nil, nil
		case errors.Is(err, fleet.ErrRevoked):
			log.Warn("fleet child: the master revoked this machine; the child has stopped and will not connect with this key again")
			return nil, nil
		}
		stopTools() // paused: the background commands and the browser end with the link
	}
}

// sleepCtx waits d and reports whether ctx is still live.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// --- status ---

// childStatus says in plain words where this machine stands with its
// master. It only reads.
func childStatus(ctx context.Context, out io.Writer, dir string) error {
	child, err := fleet.LoadChild(fleet.ChildPath(dir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("no child is enrolled here (%s): `tenant child install` enrolls this machine", dir)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w\nthis is the machine's own child: run the command from an elevated terminal", err)
	case err != nil:
		return fmt.Errorf("this machine serves nothing: its child.json does not load, and a child that cannot read its limits refuses everything\n%w", err)
	}
	only := ""
	if child.Tools != nil {
		only = fmt.Sprintf(", serving only %s", strings.Join(child.Tools, ", "))
	}
	fmt.Fprintf(out, "machine:  %s (%s)\nmaster:   %s\nceiling:  %s%s\nsandbox:  %s\n", child.Name, child.ID,
		strings.Join(child.MasterAddresses, ", "), child.Ceiling, only, child.Sandbox)
	if spec, err := childSpecFor(runtime.GOOS, "", dir, isSystemChild(dir)); err == nil {
		if ok, where := spec.installed(ctx); ok {
			fmt.Fprintf(out, "service:  %s\n", where)
		} else {
			fmt.Fprintf(out, "service:  not installed (`tenant child run --dir %q` holds the link from a terminal)\n", dir)
		}
	}
	state, connected := childStateLine(dir, child)
	fmt.Fprintf(out, "state:    %s\n", state)
	if line := childUpdateLine(dir); line != "" {
		fmt.Fprintf(out, "update:   %s\n", line)
	}
	if !connected {
		return exitError{code: 3}
	}
	return nil
}

// childStateLine is the one line that matters, and whether it says
// "connected".
func childStateLine(dir string, child fleet.Child) (string, bool) {
	if fleet.Paused(dir) {
		return "paused on this machine: it holds no link and serves nothing (`tenant child resume`)", false
	}
	st, err := fleet.ReadChildStatus(dir)
	if err != nil {
		return "not started yet: the child has not run since it was enrolled", false
	}
	since := st.Since.Local().Format("2006-01-02 15:04:05")
	// A child that is not running left its last state behind.
	if !fleet.AuditHeld(dir) && st.State != fleet.LinkRevoked {
		return "not running: no child process holds this directory (it last recorded \"" + string(st.State) + "\" at " + since + ")", false
	}
	switch st.State {
	case fleet.LinkConnected:
		line := "connected to the master at " + st.Address + " since " + since
		if st.Detail != "" {
			line += " (" + st.Detail + ")"
		}
		return line, true
	case fleet.LinkWaiting:
		line := "waiting for confirmation since " + since
		if st.Certificate != "" {
			line += ": type " + fleet.ConfirmCode(child.MasterFingerprint, st.Certificate) + " on the master (tenant fleet confirm " + child.Name + ")"
		}
		return line, false
	case fleet.LinkPaused:
		return "paused on the master since " + since + ": the operator paused this machine there", false
	case fleet.LinkRejected:
		return "the master rejected this machine (since " + since + "): its certificate is not one the master has, or the master cannot read its fleet just now. " +
			"The child keeps trying; if the machine was revoked or the master rebuilt, uninstall and enroll it again", false
	case fleet.LinkRevoked:
		return "the master rejected this machine: it is revoked (since " + since + "). The child has stopped and will not connect with this key again; " +
			"to use the machine again, `tenant child uninstall` and enroll it anew", false
	}
	return "offline since " + since + ", trying again: " + firstNonEmpty(st.Detail, "the master could not be reached"), false
}

// childUpdateLine is the last update the master sent, in words: one in
// progress, or what became of it. "" when there was none.
func childUpdateLine(dir string) string {
	st, ok, err := fleet.ReadChildUpdate(dir)
	switch {
	case err != nil:
		return "its record cannot be read: " + err.Error()
	case !ok:
		return ""
	}
	at := st.At.Local().Format("2006-01-02 15:04:05")
	switch st.State {
	case fleet.UpdateDone:
		return fmt.Sprintf("updated from %s to %s at %s", st.From, st.To, at)
	case fleet.UpdateRolledBack:
		return fmt.Sprintf("the update from %s to %s was rolled back at %s: %s", st.From, st.To, at, st.Reason)
	}
	line := fmt.Sprintf("in the middle of an update from %s to %s, since %s", st.From, st.To, at)
	if st.Step == fleet.StepCommit {
		line += "; the new version is in place, and goes back to " + st.From + " if it has not reached the master by " +
			st.Deadline.Local().Format("15:04:05")
	}
	if !fleet.AuditHeld(dir) {
		line += " (no child process is running: the next start finishes or undoes it)"
	}
	return line
}

// --- pause, resume, audit, uninstall ---

func childPause(out io.Writer, dir string, pause bool) error {
	if !exists(fleet.ChildPath(dir)) {
		return fmt.Errorf("no child is enrolled here (%s)", dir)
	}
	if err := fleet.SetPaused(dir, pause); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			err = fmt.Errorf("%w\nthis is the machine's own child: run the command from an elevated terminal", err)
		}
		return err
	}
	if pause && isSystemChild(dir) && runtime.GOOS == "windows" {
		// Writing the mark as an administrator put that account's own name on
		// the directory again (internal/secrets); take it back off.
		if err := sealSystemChild(dir); err != nil {
			return fmt.Errorf("paused, but the child's files could not be closed to this account again: %w", err)
		}
	}
	if pause {
		fmt.Fprintln(out, "Paused. Within a second this machine drops its link, stops the calls that are running and ends its background commands; "+
			"it serves nothing until `tenant child resume`, across restarts too.")
	} else {
		fmt.Fprintln(out, "Resumed. The child connects to its master again within a second, if it is running (`tenant child status`).")
	}
	return nil
}

func childAudit(out io.Writer, dir string) error {
	sum, err := fleet.VerifyAudit(dir)
	if err != nil {
		return err
	}
	if sum.Records == 0 && sum.Torn == 0 {
		fmt.Fprintf(out, "The audit log in %s is empty: this machine has not been called.\n", fleet.AuditDir(dir))
		return nil
	}
	fmt.Fprintf(out, "The audit log's chain holds: %d records (%d to %d) in %d file(s) under %s.\nLast line: %s\n",
		sum.Records, sum.First, sum.Last, sum.Files, fleet.AuditDir(dir), sum.LastHash)
	if sum.Torn > 0 {
		fmt.Fprintf(out, "%d line(s) are not records but belong where they are: writes that a crash cut short.\n", sum.Torn)
	}
	fmt.Fprintln(out, "The chain shows a change inside the log. It cannot show records cut off its end: the master holds each call's hash, and the system log has a line per record.")
	return nil
}

// childUninstall stops and removes the service and deletes the machine's
// fleet identity. The audit log and a config.json the operator wrote stay:
// the one is the record of what was done here, the other is theirs.
func childUninstall(ctx context.Context, out io.Writer, dir string) error {
	if !exists(fleet.ChildPath(dir)) && !exists(fleet.ChildIdentityPath(dir)) {
		return fmt.Errorf("no child is enrolled here (%s)", dir)
	}
	system := isSystemChild(dir)
	if system && !childElevated() {
		return exitError{2, "this is the machine's own child: `tenant child uninstall` needs an elevated terminal"}
	}
	spec, err := childSpecFor(runtime.GOOS, "", dir, system)
	if err != nil {
		return err
	}
	if ok, where := spec.installed(ctx); ok {
		if err := spec.uninstall(ctx); err != nil {
			return fmt.Errorf("the service was not removed, so nothing was deleted: %w", err)
		}
		fmt.Fprintf(out, "Removed %s.\n", where)
	}
	name := ""
	if child, err := fleet.LoadChild(fleet.ChildPath(dir)); err == nil {
		name = " " + child.Name
	}
	for _, p := range []string{fleet.ChildPath(dir), fleet.ChildPath(dir) + ".lock", fleet.ChildIdentityPath(dir),
		fleet.ChildStatusPath(dir), fleet.PausedPath(dir), fleet.ChildUpdatePath(dir)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(childDataDir(dir)); err != nil {
		return err
	}
	if system {
		_ = unregisterChildOSLog()
	}
	fmt.Fprintf(out, "Deleted this machine's fleet key and child.json. It can no longer connect; revoke it on the master too (tenant fleet revoke%s).\n", name)
	if exists(fleet.AuditDir(dir)) {
		fmt.Fprintf(out, "Kept: the audit log in %s, the record of what the master did here. Delete it when you no longer need it.\n", fleet.AuditDir(dir))
	}
	return nil
}
