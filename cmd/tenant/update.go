package main

// update.go (TEN-500): `tenant update` moves an install to a newer release
// with a way back: resolve the release on GitHub, download and verify its
// archive, smoke-test the new binary, back up the data, swap the binary with
// no gap, restart the service and confirm the hub runs the new version, or
// roll the binary back. internal/selfupdate holds each step; this file is
// the sequence, the flags and the exit codes (0 ok, 10 update available, 1
// error, 2 refused). D2 in docs/plans/install-upgrade-plan.md: serve checks
// daily and only says so; nothing is ever applied unasked.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"tenant/internal/backup"
	"tenant/internal/lifecycle"
	"tenant/internal/logs/catalog"
	"tenant/internal/privacy"
	"tenant/internal/selfupdate"
	"tenant/internal/version"
)

const updateUsage = "usage: tenant update [--check] [--version vX.Y.Z] [--yes] [--now] [--force] | --rollback"

// The health check after a restart: /api/status is read every updatePoll
// until it reports the new version or updateHealth has passed. Tests shrink
// both.
var updatePoll, updateHealth = 3 * time.Second, 90 * time.Second

// exitError is a command result with an exit status of its own: `tenant
// update` exits 10 for "update available" and 2 for a refusal. main prints
// msg when there is one and exits with code.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

type updateOpts struct {
	check, yes, now, force, rollback bool
	version                          string
}

// updateEnv is what one run acts on. cmdUpdate fills it from the real
// binary, dirs and hub; TestUpdateFlow from temp files and an httptest
// server, so no test ever touches the binary it runs in.
type updateEnv struct {
	c       *commonFlags
	exe     string // the installed binary, symlinks resolved: the file the supervisor starts
	apiBase string // "" is api.github.com
	hub     string // the hub's base URL
	token   string // the dashboard token
	repo    string // owner/name
	client  *http.Client
	stdin   io.Reader // the confirm prompt's answer
	out     io.Writer
	// restart restarts the installed service (drain-aware unless now); nil
	// when no service is installed, and the operator restarts by hand.
	restart func(ctx context.Context, now bool) error
}

func cmdUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	c := bindCommon(fs)
	var o updateOpts
	fs.BoolVar(&o.check, "check", false, "only say whether a newer release exists; exit 10 when one does")
	fs.StringVar(&o.version, "version", "", "install this tag (vX.Y.Z) instead of the latest release")
	fs.BoolVar(&o.yes, "yes", false, "don't ask before updating")
	fs.BoolVar(&o.now, "now", false, "restart without waiting for the running turn to finish")
	fs.BoolVar(&o.force, "force", false, "update a development build, or reinstall the running version")
	fs.BoolVar(&o.rollback, "rollback", false, "put the previous binary back")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), updateUsage)
	}
	if err := c.resolve(); err != nil { // loads c.lc: the dashboard address and token
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = resolveExe(exe); err != nil {
		return err
	}
	token, _ := dashboardClientToken(c.cfgDir, c.lc, true) // "" when none yet; the hub then just isn't reachable
	u := &updateEnv{c: c, exe: exe, hub: attachBaseURL("", c.lc), token: token, repo: updateRepo(),
		client: &http.Client{Timeout: 10 * time.Minute}, stdin: os.Stdin, out: os.Stdout}
	if spec, err := newServiceSpec(c, actsOnSystem(ctx)); err == nil {
		if ok, _ := spec.installed(ctx); ok {
			u.restart = func(ctx context.Context, now bool) error { return serviceRestart(ctx, spec, u.hub, u.token, now) }
		}
	}
	return runUpdate(ctx, u, o)
}

// updateRepo is where releases come from: the repository the build was
// stamped with, or TENANT_UPDATE_REPO, which lets a local build dogfood the
// real releases.
func updateRepo() string {
	if r := os.Getenv("TENANT_UPDATE_REPO"); r != "" {
		return r
	}
	return version.Repo
}

// devBuild: not a release. A dev build's binary is nobody's release asset,
// so updating it would only ever replace it with something else.
func devBuild() bool { return version.Version == "0.0.1" || version.Dirty == "true" }

func updateMarker(dataDir string) string { return filepath.Join(dataDir, "update-state.json") }

// exeName is the binary's name in a release archive (GoReleaser's flat
// archives): tenant, or tenant.exe on Windows.
func exeName() string {
	if runtime.GOOS == "windows" {
		return "tenant.exe"
	}
	return "tenant"
}

// runUpdate is the plan's eleven steps (docs/plans/install-upgrade-plan.md,
// PR B). Nothing of the install changes before step 8, so every failure up
// to the swap leaves only staging files, which the next run wipes.
func runUpdate(ctx context.Context, u *updateEnv, o updateOpts) error {
	marker := updateMarker(u.c.dataDir)
	if o.rollback {
		return u.rollback(ctx, marker, o.now)
	}
	// 11. A marker from an interrupted run: finished when the binary that
	// runs now is the one it was installing; otherwise the operator chooses.
	st, ok, err := selfupdate.ReadState(marker)
	if err != nil {
		return err
	}
	if ok {
		if st.To != version.Version {
			return exitError{2, fmt.Sprintf("the update from %s to %s was interrupted at %s; running %s. `tenant update --rollback` puts %s back; delete %s to forget it",
				st.From, st.To, st.Step, version.Version, st.From, marker)}
		}
		_ = selfupdate.ClearState(marker)
		fmt.Fprintf(u.out, "update to %s finished\n", st.To)
	}
	// 1. Refuse what isn't ours.
	if devBuild() && !o.force {
		return exitError{2, "development build; use a release build or --force"}
	}
	if u.repo == "" {
		return exitError{2, "no release source: this build names no repository (TENANT_UPDATE_REPO=owner/name sets one)"}
	}
	// 2. Resolve the release. GitHub's latest skips prereleases, so an rc is
	// reached by --version only.
	gh := os.Getenv("GITHUB_TOKEN")
	var rel selfupdate.Release
	if o.version != "" {
		rel, err = selfupdate.ByTag(ctx, u.client, u.apiBase, u.repo, o.version, gh)
	} else {
		rel, err = selfupdate.Latest(ctx, u.client, u.apiBase, u.repo, gh)
	}
	if err != nil {
		return err
	}
	newV := selfupdate.VersionOf(rel.Tag)
	if newV == version.Version && (o.check || !o.force) {
		fmt.Fprintf(u.out, "up to date (%s)\n", version.Version)
		return nil
	}
	if o.check {
		fmt.Fprintf(u.out, "update available: %s -> %s\n", version.Version, newV)
		return exitError{code: 10}
	}
	// 3. Download into a staging dir beside the binary: same filesystem, so
	// the swap's renames are atomic. Wiped first, which also cleans up after
	// an interrupted run.
	stage := filepath.Join(filepath.Dir(u.exe), ".tenant-update")
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	archive, said, err := fetchRelease(ctx, u.client, u.repo, gh, rel, runtime.GOOS, runtime.GOARCH, stage)
	if err != nil {
		return err
	}
	fmt.Fprintln(u.out, said)
	// 5. Smoke-test before touching anything: the new binary must run and
	// be the version the tag says.
	member := exeName()
	staged := filepath.Join(stage, member)
	if err := selfupdate.ExtractBinary(archive, staged, member); err != nil {
		return err
	}
	smoke, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(smoke, staged, "version").CombinedOutput(); err != nil || !strings.Contains(string(out), newV) {
		return fmt.Errorf("the new binary failed its smoke test (%s version: %v): %s", member, err, strings.TrimSpace(string(out)))
	}
	// 6. Confirm. Without a terminal there is nobody to ask: --yes.
	backupPath := preUpdateBackupPath(u.c.dataDir, version.Version)
	if !o.yes {
		if f, ok := u.stdin.(*os.File); ok && !isatty.IsTerminal(f.Fd()) {
			return exitError{2, "no terminal to confirm on; pass --yes"}
		}
		fmt.Fprintf(u.out, "Update %s -> %s? A backup goes to %s. [y/N] ", version.Version, newV, backupPath)
		ans, _ := bufio.NewReader(u.stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return exitError{2, "not confirmed"}
		}
	}
	// 7. The rollback point for the data: the binary alone cannot go back
	// once a newer version has migrated a store (the log book refuses a
	// newer schema).
	if err := preUpdateBackup(ctx, u.c, backupPath); err != nil {
		return err
	}
	fmt.Fprintf(u.out, "backup written: %s\n", backupPath)
	// 8. The marker, then the swap. A failed swap leaves the binary as it
	// was (selfupdate.Swap), so the marker goes with it.
	st = selfupdate.State{From: version.Version, To: newV, Step: "swap", Started: time.Now()}
	if err := selfupdate.WriteState(marker, st); err != nil {
		return err
	}
	hubUp := false
	if u.restart != nil {
		_, err := attachFetchStatus(ctx, &http.Client{Timeout: 10 * time.Second}, u.hub, u.token)
		hubUp = err == nil
	}
	if err := selfupdate.Swap(u.exe, staged); err != nil {
		_ = selfupdate.ClearState(marker)
		if errors.Is(err, selfupdate.ErrPrevInUse) {
			return exitError{2, err.Error()}
		}
		return err
	}
	_ = os.RemoveAll(stage)
	// 9. Restart. With no service there is nothing more to do here: the
	// operator restarts, and the new binary is simply what starts.
	if u.restart == nil {
		_ = selfupdate.ClearState(marker)
		fmt.Fprintf(u.out, "updated %s -> %s; restart Tenant to finish the update\n", version.Version, newV)
		return nil
	}
	st.Step = "restart"
	_ = selfupdate.WriteState(marker, st)
	if err := u.restart(ctx, o.now); err != nil {
		return fmt.Errorf("%w; the new binary is in place, `tenant service restart --now` finishes the update", err)
	}
	// 10. Confirm, when there was a hub to confirm with: the new version
	// must answer, or the old binary goes back. Data is never restored
	// unasked; the command for it is printed.
	if hubUp {
		st.Step = "confirm"
		_ = selfupdate.WriteState(marker, st)
		if !hubRunsVersion(ctx, u.hub, u.token, newV) {
			if err := selfupdate.Rollback(u.exe); err != nil {
				return fmt.Errorf("the hub did not come up on %s, and the rollback failed: %w", newV, err)
			}
			msg := "the hub did not come up on " + newV + "; rolled the binary back"
			if err := u.restart(ctx, true); err != nil {
				msg += " (restart failed: " + err.Error() + ")"
			}
			_ = selfupdate.ClearState(marker)
			return exitError{1, msg + "; " + restoreHint(backupPath)}
		}
	}
	_ = selfupdate.ClearState(marker)
	fmt.Fprintf(u.out, "updated %s -> %s\n", version.Version, newV)
	return nil
}

// fetchRelease is steps 3 and 4 for one target: the archive release rel
// publishes for goos and goarch, its checksums and, when the release has
// one, the signature bundle, downloaded into stage; then verified, the
// checksum always and the signature when cosign is on PATH and the release
// carries the bundle, failing closed. `tenant update` takes its own target;
// `tenant fleet update` takes the machine's (fleetupdate.go), so a child is
// sent nothing this Tenant would not install on itself. said is the line
// that tells the operator what was verified.
func fetchRelease(ctx context.Context, client *http.Client, repo, gh string, rel selfupdate.Release, goos, goarch, stage string) (archive, said string, err error) {
	asset := selfupdate.AssetName(selfupdate.VersionOf(rel.Tag), goos, goarch)
	for _, name := range []string{asset, "checksums.txt"} {
		url, ok := rel.Assets[name]
		if !ok {
			return "", "", fmt.Errorf("release %s has no %s", rel.Tag, name)
		}
		if err := selfupdate.Download(ctx, client, url, gh, filepath.Join(stage, name)); err != nil {
			return "", "", err
		}
	}
	archive, sums := filepath.Join(stage, asset), filepath.Join(stage, "checksums.txt")
	bundle := ""
	if url, ok := rel.Assets["checksums.txt.sigstore.json"]; ok {
		bundle = filepath.Join(stage, "checksums.txt.sigstore.json")
		if err := selfupdate.Download(ctx, client, url, gh, bundle); err != nil {
			return "", "", err
		}
	}
	if err := selfupdate.Verify(archive, sums); err != nil {
		return "", "", err
	}
	switch {
	case bundle != "" && selfupdate.HaveCosign():
		if err := selfupdate.CosignVerify(ctx, runCmd, sums, bundle, repo); err != nil {
			return "", "", err
		}
		return archive, "verified checksum and signature", nil
	case bundle == "":
		return archive, "verified checksum only (the release carries no signature bundle)", nil
	}
	return archive, "verified checksum only (install cosign to verify the signature)", nil
}

// rollback is --rollback: the previous binary back, a restart, and the
// data-restore command, since a newer version may have migrated a store.
func (u *updateEnv) rollback(ctx context.Context, marker string, now bool) error {
	if err := selfupdate.Rollback(u.exe); err != nil {
		return err
	}
	if u.restart != nil {
		if err := u.restart(ctx, now); err != nil {
			return err
		}
	}
	_ = selfupdate.ClearState(marker)
	path := "<the pre-update backup>"
	if m, _ := filepath.Glob(filepath.Join(preUpdateBackupDir(u.c.dataDir), "pre-update-*")); len(m) > 0 {
		path = m[len(m)-1]
	}
	fmt.Fprintf(u.out, "rolled the binary back; %s\n", restoreHint(path))
	return nil
}

func restoreHint(backupPath string) string {
	return "if the new version migrated data, restore with: tenant service stop && tenant backup restore " + quoteArg(backupPath) + " --yes && tenant service start"
}

// hubRunsVersion polls /api/status until it reports want. The old process
// may still answer right after the restart, so a mismatch is waited out,
// not failed.
func hubRunsVersion(ctx context.Context, base, token, want string) bool {
	httpc := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(updateHealth)
	for {
		if st, err := attachFetchStatus(ctx, httpc, base, token); err == nil && st.Version == want {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(updatePoll):
		}
	}
}

// The rollback point lives beside the data dir, not in it: backup.Create
// refuses a backup inside the directories it captures, and a restore swaps
// the data dir away.
func preUpdateBackupDir(dataDir string) string { return dataDir + "-backups" }

func preUpdateBackupPath(dataDir, from string) string {
	return filepath.Join(preUpdateBackupDir(dataDir), "pre-update-"+from+".tenant-backup")
}

// preUpdateBackup writes the rollback point, unencrypted (no prompt;
// credentials are left out, and migrations never touch them), replacing the
// one from the last update. It is filed like any backup, so the audit trail
// and `tenant privacy status` know it exists.
//
// ponytail: one rollback point; keep N when someone needs to go back two
// versions.
func preUpdateBackup(ctx context.Context, c *commonFlags, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	old, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), "pre-update-*"))
	for _, p := range old {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	managed, err := managedSecrets(c.cfgDir, c.dataDir)
	if err != nil {
		return err
	}
	res, err := backup.Create(ctx, dest, backupOptions(c, managed, false))
	if err != nil {
		return err
	}
	_ = privacy.NoteBackup(privacy.Data{DataDir: c.dataDir}, res.Path, res.Manifest.CreatedAt)
	_ = catalog.BackupWritten.Audit(ctx, "path", res.Path, "bytes", res.Bytes, "encrypted", false, "actor", actorOperator, "surface", surfaceCLI)
	return nil
}

// The daily check (D2: check, never apply).

// startUpdateCheck files a marker an interrupted update left behind, then
// looks for a newer release 5 minutes after boot and daily after that, an
// hour of jitter either way so a fleet does not poll in step. One
// UpdateAvailable event per new tag; the existing alert rules can turn it
// into a message. Off with "update_check": false.
func startUpdateCheck(ctx context.Context, life *lifecycle.Group, c *commonFlags) {
	reportInterruptedUpdate(ctx, c.dataDir)
	if !updateCheckOn(c.lc) {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	life.StartFunc(lifecycle.Drain, "update check", lifecycle.Subsystem, "system", cancel)
	go func() {
		announced := ""
		wait := 5 * time.Minute
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			updateCheck(ctx, c.lc, nil, "", &announced)
			wait = 23*time.Hour + rand.N(2*time.Hour)
		}
	}()
}

// updateCheckOn: the check runs for a release build that names a
// repository, unless "update_check": false opts out.
func updateCheckOn(lc *launchConfig) bool {
	return !(lc != nil && lc.UpdateCheck != nil && !*lc.UpdateCheck) && !devBuild() && updateRepo() != ""
}

// updateCheck is one check. A tag newer than the running version and not
// yet announced writes UpdateAvailable. A network error is not news, so it
// is silent.
func updateCheck(ctx context.Context, lc *launchConfig, client *http.Client, apiBase string, announced *string) {
	if !updateCheckOn(lc) {
		return
	}
	rel, err := selfupdate.Latest(ctx, client, apiBase, updateRepo(), os.Getenv("GITHUB_TOKEN"))
	if err != nil || selfupdate.VersionOf(rel.Tag) == version.Version || rel.Tag == *announced {
		return
	}
	*announced = rel.Tag
	catalog.UpdateAvailable.Log(ctx, "latest", rel.Tag, "current", version.Version)
}

// reportInterruptedUpdate files the marker of an update that never
// finished, once per boot; `tenant update` finishes it or offers --rollback.
func reportInterruptedUpdate(ctx context.Context, dataDir string) {
	if st, ok, _ := selfupdate.ReadState(updateMarker(dataDir)); ok {
		catalog.UpdateInterrupted.Log(ctx, "from", st.From, "to", st.To, "step", st.Step, "running", version.Version)
	}
}

// checkUpdate (doctor): an update that was interrupted, else whether a newer
// release exists. Doctor has no INFO status: a newer release is reported OK
// with the detail, since it is not a fault.
func checkUpdate(ctx context.Context, e *doctorEnv) checkResult {
	st, ok, err := selfupdate.ReadState(updateMarker(e.c.dataDir))
	if err != nil {
		return checkResult{Status: statusWarn, Detail: err.Error()}
	}
	if ok {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("the update from %s to %s was interrupted at %s; running %s", st.From, st.To, st.Step, version.Version),
			Fix:    "tenant update (finishes it, or offers --rollback)"}
	}
	repo := updateRepo()
	if devBuild() || repo == "" {
		return checkResult{Status: statusSkip, Detail: "development build; no release to compare with"}
	}
	rel, err := selfupdate.Latest(ctx, e.httpc, "", repo, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		return checkResult{Status: statusSkip, Detail: "offline: " + err.Error()}
	}
	if selfupdate.VersionOf(rel.Tag) == version.Version {
		return checkResult{Status: statusOK, Detail: "up to date (" + version.Version + ")"}
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("update available: %s (running %s)", rel.Tag, version.Version), Fix: "tenant update"}
}
