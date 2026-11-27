package main

// fleet_update_e2e_test.go: `tenant fleet update` with every piece a process
// of its own (TEN-533; docs/plans/gap6-fleet-plan.md, section 8). The master
// is a real `tenant serve`, the machine a real `tenant child run` started
// from a copy of this test binary, and the release a GitHub of the test's own
// on loopback, whose archives hold further copies of this test binary marked
// with the version they say they are (and, for the broken builds, how they
// misbehave). No test replaces a binary of this computer: only the copies in
// the test's temp dir are swapped. The harness is the service manager: it
// starts the child again when a test kills it, as a unit would.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/control"
	"tenant/internal/fleet"
	"tenant/internal/selfupdate"
	"tenant/internal/version"
)

// e2eMark ends a copy of this test binary that stands in for a release:
// "\nTENANT-E2E-BUILD version=V behave=B\n". Appended data is ignored by the
// loaders of all three systems, so the copy still runs.
const e2eMark = "\nTENANT-E2E-BUILD "

// e2eReadMark is the mark at the end of this process's own binary.
func e2eReadMark() (ver, behave string) {
	exe, err := os.Executable()
	if err != nil {
		return "", ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() < 256 {
		return "", ""
	}
	tail := make([]byte, 256)
	if _, err := f.ReadAt(tail, fi.Size()-256); err != nil {
		return "", ""
	}
	i := bytes.LastIndex(tail, []byte(e2eMark))
	if i < 0 {
		return "", ""
	}
	for _, kv := range strings.Fields(string(tail[i+len(e2eMark):])) {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "version":
			ver = v
		case "behave":
			behave = v
		}
	}
	return ver, behave
}

// e2eUpdateSeams is what a helper process needs for an update: the version
// its binary is marked with, the test's release source, shorter waits, and
// a way to start itself again (the smoke test, the trial, the restart) as a
// helper process with the same orders. A marked build that misbehaves does
// it here, as `child run`: exits at once, or runs and never reaches the
// master until whoever started it lets go.
func e2eUpdateSeams(spec e2eSpec) {
	ver, behave := e2eReadMark()
	if ver != "" {
		version.Version = ver
	}
	if v := os.Getenv("TENANT_E2E_RELEASE_API"); v != "" {
		fleetReleaseAPI = v
	}
	if d, err := time.ParseDuration(os.Getenv("TENANT_E2E_CHILD_HEALTH")); err == nil {
		childUpdateHealth = d
	}
	if d, err := time.ParseDuration(os.Getenv("TENANT_E2E_UPDATE_WAIT")); err == nil {
		fleetUpdateWait = d
	}
	// A child on the default rule verifies a release (decision A71) as a
	// release build of test/tenant would: the identity is what its binary
	// carries, here stood in for, and the signature is the test's own kind
	// (childrelease_test.go), since Sigstore cannot be reached from a test.
	if repo := os.Getenv("TENANT_E2E_CHILD_RELEASE"); repo != "" {
		childReleaseRepo, childCosign = func() string { return repo }, fakeCosign
	}
	// A child whose binary carries the project key (TEN-542), stamped as a
	// release build stamps it, and with no cosign on its machine.
	if keys := os.Getenv("TENANT_E2E_CHILD_KEYS"); keys != "" {
		version.ReleaseKeys = keys
		childCosign = func(context.Context, ...string) error {
			return errors.New("cosign is not installed on this machine, and the release's signature is verified with it")
		}
	}
	childCommand = func(ctx context.Context, exe string, args ...string) *exec.Cmd {
		sub := e2eSpec{Argv: args, Env: spec.Env, Scratch: spec.Scratch, Shipped: spec.Shipped}
		// The child that takes this process's place on Linux and macOS (exec)
		// inherits the test's hold on standard input; a trial and a kept child
		// read their own.
		sub.Hold = len(args) >= 2 && args[0] == "child" && args[1] == "run" && !slices.Contains(args, "--trial") && !slices.Contains(args, "--kept")
		raw, _ := json.Marshal(sub)
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestFleetE2EProcess$")
		cmd.Env = append(os.Environ(), e2eProcEnv+"="+base64.RawURLEncoding.EncodeToString(raw))
		hideWindow(cmd)
		return cmd
	}
	if len(spec.Argv) >= 2 && spec.Argv[0] == "child" && spec.Argv[1] == "run" {
		switch behave {
		case "exit":
			fmt.Fprintln(os.Stderr, "e2e build "+ver+": exits at once")
			os.Exit(1)
		case "hang":
			fmt.Fprintln(os.Stderr, "e2e build "+ver+": runs and never reaches the master")
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}
	}
}

// e2eBuild writes a copy of this test binary into dir, marked as version v
// that behaves as behave ("" for a good build), and returns its path.
func e2eBuild(t *testing.T, dir, v, behave string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	path := filepath.Join(dir, exeName())
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(dst, src)
	if err == nil {
		_, err = fmt.Fprintf(dst, "%sversion=%s behave=%s\n", e2eMark, v, behave)
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// e2eMarkOf is the version a binary on disk is marked with.
func e2eMarkOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.LastIndex(b, []byte(e2eMark+"version="))
	if i < 0 {
		return ""
	}
	v, _, _ := strings.Cut(string(b[i+len(e2eMark)+len("version="):]), " ")
	return v
}

// e2eReleases is a GitHub of the test's own: release documents by tag, and
// each release's archive for this system and its checksums.
type e2eReleases struct {
	t   *testing.T
	dir string
	srv *httptest.Server

	mu   sync.Mutex
	tags map[string]bool
}

func newE2EReleases(t *testing.T, dir string) *e2eReleases {
	t.Helper()
	r := &e2eReleases{t: t, dir: dir, tags: map[string]bool{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *e2eReleases) serve(w http.ResponseWriter, req *http.Request) {
	const repo = "/repos/test/tenant/releases/tags/"
	switch {
	case strings.HasPrefix(req.URL.Path, repo):
		tag := strings.TrimPrefix(req.URL.Path, repo)
		r.mu.Lock()
		ok := r.tags[tag]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		var assets []map[string]string
		for _, name := range []string{selfupdate.AssetName(selfupdate.VersionOf(tag), runtime.GOOS, runtime.GOARCH), "checksums.txt", "checksums.txt.sigstore.json", selfupdate.SignatureAsset} {
			if _, err := os.Stat(filepath.Join(r.dir, tag, name)); err != nil {
				continue // a release with no signature bundle
			}
			assets = append(assets, map[string]string{"name": name, "url": r.srv.URL + "/dl/" + tag + "/" + name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": assets})
	case strings.HasPrefix(req.URL.Path, "/dl/"):
		rest := strings.TrimPrefix(req.URL.Path, "/dl/")
		if strings.Contains(rest, "..") {
			http.NotFound(w, req)
			return
		}
		http.ServeFile(w, req, filepath.Join(r.dir, filepath.FromSlash(rest)))
	default:
		http.NotFound(w, req)
	}
}

// publish makes release tag out of the binary at bin: its archive, packed
// as GoReleaser packs this system's (a zip on Windows, else a gzip'd tar;
// stored and fast, for size), and checksums.txt.
func (r *e2eReleases) publish(tag, bin string) {
	r.t.Helper()
	dir := filepath.Join(r.dir, tag)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.t.Fatal(err)
	}
	name := selfupdate.AssetName(selfupdate.VersionOf(tag), runtime.GOOS, runtime.GOARCH)
	path := filepath.Join(dir, name)
	if err := packRelease(path, bin); err != nil {
		r.t.Fatal(err)
	}
	sum, err := selfupdate.SHA256(path)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sum+"  "+name+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	if strings.HasPrefix(tag, "v6.") { // signed with the project key e2eProjectKey; v6.0.1's signature has one byte changed
		sig := projectSign(e2eProjectKey, []byte(sum+"  "+name+"\n"))
		if tag == "v6.0.1" { // another base64 character: the text still reads, the signature is wrong
			if sig[20] == 'A' {
				sig[20] = 'B'
			} else {
				sig[20] = 'A'
			}
		}
		if err := os.WriteFile(filepath.Join(dir, selfupdate.SignatureAsset), sig, 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	if strings.HasPrefix(tag, "v5.") { // the releases the default-rule machine is sent are signed, but for one
		if tag != "v5.0.1" {
			if err := os.WriteFile(filepath.Join(dir, "checksums.txt.sigstore.json"), fakeSign("test/tenant", []byte(sum+"  "+name+"\n")), 0o600); err != nil {
				r.t.Fatal(err)
			}
		}
	}
	r.mu.Lock()
	r.tags[tag] = true
	r.mu.Unlock()
}

// packRelease writes bin as the one member of a release archive at path.
func packRelease(path, bin string) error {
	src, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	if strings.HasSuffix(path, ".zip") {
		zw := zip.NewWriter(out)
		w, err := zw.CreateHeader(&zip.FileHeader{Name: exeName(), Method: zip.Store})
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, src); err != nil {
			return err
		}
		return zw.Close()
	}
	gz, _ := gzip.NewWriterLevel(out, gzip.BestSpeed)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: exeName(), Mode: 0o755, Size: fi.Size(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, src); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// e2eUpdateFleet is a master and one machine "alpha", the machine run from a
// copy of this test binary marked 1.0.0, both dialing through a wire so the
// master can be killed and started again on another port.
type e2eUpdateFleet struct {
	h     *e2eHost
	m     *e2eMaster
	wire  *e2eWire
	rel   *e2eReleases
	alpha *e2eChild
	bin   string // alpha's binary
}

func newE2EUpdateFleet(t *testing.T) *e2eUpdateFleet {
	t.Helper()
	if testing.Short() {
		t.Skip("starts a master and a machine as processes, and updates the machine")
	}
	f := &e2eUpdateFleet{h: thisHost(t)}
	f.rel = newE2EReleases(t, f.h.join("releases"))
	f.wire = newE2EWire(t)
	f.m = f.h.master(true)
	f.m.env = map[string]string{"TENANT_UPDATE_REPO": "test/tenant", "TENANT_E2E_RELEASE_API": f.rel.srv.URL, "TENANT_E2E_UPDATE_WAIT": "3m"}
	f.m.serve()
	f.m.init(f.wire.addr)
	f.wire.to(f.m.fleet)
	f.alpha = f.machine(t, "alpha", false, "")
	f.bin = f.alpha.host.bin
	return f
}

// machine enrolls, confirms and runs a machine from its own copy of this
// binary, marked 1.0.0. It takes its master's hash (`--updates master`),
// as a development build must; release is a machine on the default rule
// whose binary carries the release identity test/tenant, and keys the
// project key(s) it carries besides (TEN-542; "" for none).
func (f *e2eUpdateFleet) machine(t *testing.T, name string, release bool, keys string) *e2eChild {
	t.Helper()
	host := *f.h
	host.bin = e2eBuild(t, f.h.join("bin-"+name), "1.0.0", "")
	flags := []string{"--updates", "master"}
	env := map[string]string{"TENANT_E2E_CHILD_HEALTH": "15s"}
	if release {
		flags, env["TENANT_E2E_CHILD_RELEASE"] = nil, "test/tenant"
	}
	if keys != "" {
		env["TENANT_E2E_CHILD_KEYS"] = keys
	}
	c := f.m.enroll(&host, name, flags...)
	c.env = env
	c.confirm()
	c.run()
	c.state("connected to the master")
	return c
}

// update runs `tenant fleet update` against the running master's dashboard.
func (f *e2eUpdateFleet) update(args ...string) (string, int) {
	f.m.t.Helper()
	out, errOut, exit := f.m.cli(nil, append(append([]string{"fleet", "update"}, args...), "--hub", f.m.dash)...)
	return out + errOut, exit
}

// reported is the version the master's API shows alpha reporting.
func (f *e2eUpdateFleet) reported(t *testing.T, id string) string {
	t.Helper()
	var v apiFleet
	if code, body := f.m.api(http.MethodGet, "/api/fleet", nil, &v); code != http.StatusOK {
		t.Fatalf("/api/fleet: %d %s", code, body)
	}
	for _, m := range v.Machines {
		if m.ID == id {
			return m.Reported.Version
		}
	}
	return ""
}

// rolledBackTo checks that an update that went wrong left alpha connected,
// on the binary and the version it had.
func (f *e2eUpdateFleet) rolledBackTo(t *testing.T, ver, why string) {
	t.Helper()
	f.alpha.state("rolled back")
	st := f.alpha.state("connected to the master")
	if !strings.Contains(st, why) || !strings.Contains(st, "rolled back") {
		t.Errorf("tenant child status after a rollback:\n%s", st)
	}
	if got := e2eMarkOf(t, f.bin); got != ver {
		t.Errorf("the installed binary is marked %q, want %q", got, ver)
	}
	if _, err := os.Stat(childStageDir(f.bin)); err == nil {
		t.Errorf("the unpacked binary is still there after the rollback")
	}
	for deadline := time.Now().Add(e2eWait); f.reported(t, f.alpha.id) != `"`+ver+`"`; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the master shows alpha on %s, want %q", f.reported(t, f.alpha.id), ver)
		}
	}
}

func TestFleetUpdateRealProcesses(t *testing.T) {
	f := newE2EUpdateFleet(t)
	m, h, alpha := f.m, f.h, f.alpha
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		t.Run(name, fn)
		if t.Failed() {
			t.FailNow()
		}
	}
	builds := h.join("builds")

	step("refusals before anything is sent", func(t *testing.T) {
		f.rel.publish("v0.9.0", e2eBuild(t, filepath.Join(builds, "0.9.0"), "0.9.0", ""))
		f.rel.publish("v1.0.0", e2eBuild(t, filepath.Join(builds, "1.0.0"), "1.0.0", ""))
		if out, exit := f.update("alpha", "--version", "v0.9.0"); exit == 0 || !strings.Contains(out, "older than") || !strings.Contains(out, "--allow-older") {
			t.Errorf("a downgrade without the flag: exit %d\n%s", exit, out)
		}
		if out, exit := f.update("alpha", "--version", "v1.0.0"); exit != 0 || !strings.Contains(out, "already runs 1.0.0") {
			t.Errorf("the version it runs: exit %d\n%s", exit, out)
		}
		if out, exit := f.update("alpha", "--version", "banana"); exit == 0 || !strings.Contains(out, "not a version") {
			t.Errorf("a version that is not one: exit %d\n%s", exit, out)
		}
		if out, exit := f.update("nobody"); exit == 0 || !strings.Contains(out, "no machine") {
			t.Errorf("an unknown machine: exit %d\n%s", exit, out)
		}
		if parts, _ := filepath.Glob(filepath.Join(childDataDir(alpha.dir), "update", "*")); len(parts) != 0 {
			t.Errorf("a refused update sent the machine something: %v", parts)
		}
	})

	step("a new binary that exits at once is rolled back", func(t *testing.T) {
		f.rel.publish("v2.0.1", e2eBuild(t, filepath.Join(builds, "2.0.1"), "2.0.1", "exit"))
		out, exit := f.update("alpha", "--version", "v2.0.1")
		if exit == 0 || !strings.Contains(out, "alpha: rolled back to \"1.0.0\"") || !strings.Contains(out, "stopped before it reached the master") {
			t.Errorf("exit %d\n%s", exit, out)
		}
		t.Logf("the update said:\n%s", out)
		f.rolledBackTo(t, "1.0.0", "stopped before it reached the master")
	})

	step("a new binary that never reaches the master is rolled back after N seconds", func(t *testing.T) {
		f.rel.publish("v2.0.2", e2eBuild(t, filepath.Join(builds, "2.0.2"), "2.0.2", "hang"))
		start := time.Now()
		out, exit := f.update("alpha", "--version", "v2.0.2")
		if exit == 0 || !strings.Contains(out, "did not reach the master within 15s") {
			t.Errorf("exit %d\n%s", exit, out)
		}
		if took := time.Since(start); took < 15*time.Second {
			t.Errorf("rolled back after %s, before its 15 s were up", took)
		}
		t.Logf("the update said:\n%s", out)
		f.rolledBackTo(t, "1.0.0", "did not reach the master within 15s")
	})

	var startFile, endFile string
	var started time.Time
	// bgRuns is how long the background command runs. It has to outlast the
	// update that is to end it, or its end says nothing about the restart: an
	// update takes some ten seconds, and over a minute under the race
	// detector on CI's Linux runner, where at 45 seconds the command had
	// ended by itself and the test failed with "the background command
	// outlived the update".
	bgRuns := 45 * time.Second
	if raceEnabled {
		bgRuns = 2 * time.Minute
	}
	step("a command is running on the machine when the update starts", func(t *testing.T) {
		startFile, endFile = h.join("bg-start"), h.join("bg-end")
		res, cards := m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.slow(startFile, int(bgRuns/time.Second), endFile, "never-printed"), "background": true}, "approve")
		if len(cards) != 1 || !strings.Contains(res, "background") {
			t.Fatalf("the background command: %d cards\n%s", len(cards), res)
		}
		for deadline := time.Now().Add(e2eWait); ; time.Sleep(50 * time.Millisecond) {
			if _, err := os.Stat(startFile); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the background command did not start")
			}
		}
		started = time.Now()
	})

	step("an update comes back on the new version", func(t *testing.T) {
		f.rel.publish("v2.0.0", e2eBuild(t, filepath.Join(builds, "2.0.0"), "2.0.0", ""))
		out, exit := f.update("alpha", "--version", "v2.0.0")
		restarted := time.Since(started) // the machine is back by now, so it restarted before this
		for _, want := range []string{"alpha: fetching v2.0.0 for " + runtime.GOOS + "/" + runtime.GOARCH, "alpha: fetched tenant_2.0.0_", "verified checksum only",
			"the machine checked its SHA-256 and is applying it. The calls it is running finish first, and new ones wait until it is back. Its background commands end at the restart",
			"alpha: back on the new version 2.0.0"} {
			if !strings.Contains(out, want) {
				t.Errorf("the update's output lacks %q", want)
			}
		}
		if exit != 0 {
			t.Fatalf("exit %d\n%s", exit, out)
		}
		// "reached the master in its trial run" is said when the master sees the
		// trial's link, which may come and go between two of its looks.
		t.Logf("the update said:\n%s", out)
		if st := alpha.state("connected to the master"); !strings.Contains(st, "updated from 1.0.0 to 2.0.0") {
			t.Errorf("tenant child status:\n%s", st)
		}
		if got := e2eMarkOf(t, f.bin); got != "2.0.0" {
			t.Errorf("the installed binary is marked %q", got)
		}
		if got := e2eMarkOf(t, selfupdate.PrevPath(f.bin)); got != "1.0.0" {
			t.Errorf("the previous binary, kept for a rollback, is marked %q", got)
		}
		if got := f.reported(t, alpha.id); got != `"2.0.0"` {
			t.Errorf("the master shows alpha on %s", got)
		}
		// The new version serves.
		res, _ := m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.say("on-the-new-version")}, "approve")
		if !strings.Contains(res, "\non-the-new-version") {
			t.Errorf("a command on the updated machine:\n%s", res)
		}
		// The command that was running ended at the restart.
		if restarted >= bgRuns {
			t.Fatalf("the update took %s and the background command runs for %s: it may have ended by itself, which shows nothing about the restart (see bgRuns)", restarted.Round(time.Second), bgRuns)
		}
		time.Sleep(time.Until(started.Add(bgRuns + 5*time.Second)))
		if _, err := os.Stat(endFile); err == nil {
			t.Errorf("the background command outlived the update (the machine was back %s after the command started, and the command runs for %s)", restarted.Round(time.Second), bgRuns)
		}
	})

	step("an offline machine is refused, and --all goes on past it", func(t *testing.T) {
		beta := f.machine(t, "beta", false, "")
		beta.proc.stop()
		for deadline := time.Now().Add(e2eWait); ; time.Sleep(100 * time.Millisecond) {
			var v apiFleet
			m.api(http.MethodGet, "/api/fleet", nil, &v)
			if i := slices.IndexFunc(v.Machines, func(x apiFleetMachine) bool { return x.ID == beta.id }); i >= 0 && !v.Machines[i].Online {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("beta is still online")
			}
		}
		out, exit := f.update("--all", "--version", "v2.0.0")
		if exit == 0 || !strings.Contains(out, "alpha: already runs 2.0.0") || !strings.Contains(out, "beta: refused: Nothing was sent: beta is offline") ||
			!strings.Contains(out, "Nothing is queued") || !strings.Contains(out, "1 of 2 machines were not updated") {
			t.Errorf("--all with beta offline: exit %d\n%s", exit, out)
		}
		if out, exit := f.update("beta", "--version", "v2.0.0"); exit == 0 || !strings.Contains(out, "offline") {
			t.Errorf("beta alone: exit %d\n%s", exit, out)
		}
	})

	step("the master is killed mid-transfer and the machine is unharmed", func(t *testing.T) {
		f.rel.publish("v3.0.0", e2eBuild(t, filepath.Join(builds, "3.0.0"), "3.0.0", ""))
		cli := h.start("fleet update", e2eSpec{Argv: []string{"fleet", "update", "alpha", "--version", "v3.0.0", "--hub", m.dash, "--config", m.cfg, "--data", m.data}})
		part := ""
		for deadline := time.Now().Add(e2eWait); part == ""; time.Sleep(10 * time.Millisecond) {
			parts, _ := filepath.Glob(filepath.Join(childDataDir(alpha.dir), "update", "*.part"))
			if len(parts) == 1 {
				if fi, err := os.Stat(parts[0]); err == nil && fi.Size() > 0 {
					part = parts[0]
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("no piece of the archive arrived\n%s", cli.said())
			}
		}
		m.proc.kill()
		cli.ended("the command line gives up on a master that is gone")
		if alpha.proc.exited() {
			t.Fatalf("the machine's child ended with its master\n%s", alpha.proc.said())
		}
		var arrived int64
		if fi, err := os.Stat(part); err == nil {
			arrived = fi.Size()
		}
		if got := e2eMarkOf(t, f.bin); got != "2.0.0" {
			t.Errorf("the installed binary is marked %q after a broken transfer", got)
		}
		if st, _, _ := fleet.ReadChildUpdate(alpha.dir); st.State != fleet.UpdateDone || st.To != "2.0.0" {
			t.Errorf("child-update.json after a broken transfer: %+v", st)
		}
		// The master comes back; the machine finds it, and the update goes on
		// from what had arrived.
		m.serve()
		f.wire.to(m.fleet)
		alpha.state("connected to the master")
		// TEN-552: the master that came back has the update it was killed in
		// the middle of, ended where it stood, and sends nothing by itself.
		if u := f.lastUpdate(t, alpha.id); u == nil || u.Running || u.Stage != fleetUpdFailed || len(u.Lines) == 0 ||
			!strings.Contains(u.Lines[len(u.Lines)-1], "this Tenant stopped while the archive was on its way to the machine") {
			t.Errorf("the restarted master's record of the update it was killed in: %+v", u)
		}
		if fi, err := os.Stat(part); err != nil || fi.Size() != arrived {
			t.Errorf("the machine held %d bytes when its master was killed, and holds something else now that it is back (%v)", arrived, err)
		}
		out, exit := f.update("alpha", "--version", "v3.0.0")
		if exit != 0 || !strings.Contains(out, "from an earlier try") || !strings.Contains(out, "back on the new version 3.0.0") {
			t.Errorf("the update again: exit %d\n%s", exit, out)
		}
	})

	step("the child is killed mid-apply and comes back on a working binary", func(t *testing.T) {
		f.rel.publish("v4.0.0", e2eBuild(t, filepath.Join(builds, "4.0.0"), "4.0.0", "hang"))
		cli := h.start("fleet update", e2eSpec{Argv: []string{"fleet", "update", "alpha", "--version", "v4.0.0", "--hub", m.dash, "--config", m.cfg, "--data", m.data}})
		for deadline := time.Now().Add(e2eWait); ; time.Sleep(20 * time.Millisecond) {
			if st, _, _ := fleet.ReadChildUpdate(alpha.dir); st.Step == fleet.StepTrial && st.To == "4.0.0" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the trial did not start\n%s", cli.said())
			}
		}
		alpha.proc.kill()
		alpha.run() // the unit starts it again
		f.rolledBackTo(t, "3.0.0", "interrupted during the trial run")
		cli.ended("the command line hears of the rollback")
		if !strings.Contains(cli.out.String(), "alpha: rolled back to \"3.0.0\"") {
			t.Errorf("the command line's account:\n%s", cli.said())
		}
		res, _ := m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.say("after-the-kill")}, "approve")
		if !strings.Contains(res, "\nafter-the-kill") {
			t.Errorf("a command after the rollback:\n%s", res)
		}
	})

	// TEN-552: an update costs the calls to the machine nothing. One turn makes
	// two calls: a command that takes ten seconds, and one after it. The update
	// starts while the first runs, so the machine says "applying" in the middle
	// of it: it must finish and be answered, where before it was cut off a
	// second later with an outcome nobody knew. The second is made while the
	// machine restarts: it must wait on the master and be answered by the new
	// version, where before it failed as offline.
	step("an update costs a running call and the next one nothing", func(t *testing.T) {
		f.rel.publish("v3.1.0", e2eBuild(t, filepath.Join(builds, "3.1.0"), "3.1.0", ""))
		slowStart, slowEnd := h.join("call-start"), h.join("call-end")
		var cli *e2eProc
		launched := make(chan struct{})
		go func() { // the operator, updating the machine while the agent works on it
			defer close(launched)
			for deadline := time.Now().Add(e2eWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if _, err := os.Stat(slowStart); err == nil {
					cli = h.start("fleet update", e2eSpec{Argv: []string{"fleet", "update", "alpha", "--version", "v3.1.0", "--hub", m.dash, "--config", m.cfg, "--data", m.data}})
					return
				}
			}
		}()
		turn := m.turn(e2eScript(
			e2eCall("fleet_exec", map[string]any{"host": "alpha", "command": h.slow(slowStart, 10, slowEnd, "ran-through-the-update")}),
			e2eCall("fleet_exec", map[string]any{"host": "alpha", "command": h.say("after-the-update")}),
		), func(control.PendingApproval) string { return "approve" })
		<-launched
		if cli == nil {
			t.Fatalf("the slow command did not start\n%s", strings.Join(turn.Results, "\n---\n"))
		}
		cli.ended("the update of a machine that was in the middle of a call")
		out := cli.said()
		t.Logf("the update said:\n%s\nthe turn's results:\n%s", out, strings.Join(turn.Results, "\n---\n"))
		if !strings.Contains(out, "alpha: back on the new version 3.1.0") {
			t.Errorf("the update:\n%s", out)
		}
		if len(turn.Results) != 2 {
			t.Fatalf("the turn was handed %d results", len(turn.Results))
		}
		if !strings.Contains(turn.Results[0], "\nran-through-the-update") {
			t.Errorf("the call that was running when the update came:\n%s", turn.Results[0])
		}
		if _, err := os.Stat(slowEnd); err != nil {
			t.Errorf("the command that was running when the update came did not run to its end: %v", err)
		}
		if !strings.Contains(turn.Results[1], "\nafter-the-update") {
			t.Errorf("the call made while the machine restarted:\n%s", turn.Results[1])
		}
		for i, res := range turn.Results {
			if strings.Contains(res, "offline") || strings.Contains(res, "is not known") {
				t.Errorf("result %d speaks of a machine that was away:\n%s", i+1, res)
			}
		}
		if got := e2eMarkOf(t, f.bin); got != "3.1.0" {
			t.Errorf("the installed binary is marked %q", got)
		}
		alpha.state("connected to the master")
	})

	// TEN-552: a build of the operator's own, sent in place of a release
	// (`tenant fleet update --build`), as scripts/devbuild packs one: the
	// archive a release would carry, under a development version. alpha takes
	// its master's hash, so it takes the build; nothing is fetched for it.
	const devVersion = "3.2.0-dev.20261126193000.e56d774"
	devArchive := filepath.Join(builds, "dev", selfupdate.AssetName(devVersion, runtime.GOOS, runtime.GOARCH))
	step("a build of your own goes to a machine that trusts its master", func(t *testing.T) {
		if err := packRelease(devArchive, e2eBuild(t, filepath.Join(builds, "dev"), devVersion, "")); err != nil {
			t.Fatal(err)
		}
		if out, exit := f.update("alpha", "--build", devArchive, "--version", "v9.9.9"); exit == 0 || !strings.Contains(out, "do not go together") {
			t.Errorf("--build with --version: exit %d\n%s", exit, out)
		}
		if out, exit := f.update("alpha", "--build", f.bin); exit == 0 || !strings.Contains(out, "not a build's archive by its name") {
			t.Errorf("a bare binary as a build: exit %d\n%s", exit, out)
		}
		otherOS := "linux"
		if runtime.GOOS == "linux" {
			otherOS = "darwin"
		}
		wrong := filepath.Join(builds, "dev", selfupdate.AssetName(devVersion, otherOS, runtime.GOARCH))
		if err := os.WriteFile(wrong, []byte("another system's build"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, exit := f.update("alpha", "--build", wrong); exit == 0 || !strings.Contains(out, "is a build for "+otherOS+"/"+runtime.GOARCH) || !strings.Contains(out, "nothing was sent") {
			t.Errorf("another system's build: exit %d\n%s", exit, out)
		}
		if parts, _ := filepath.Glob(filepath.Join(childDataDir(alpha.dir), "update", "*")); len(parts) != 0 {
			t.Errorf("a refused build sent the machine something: %v", parts)
		}

		out, exit := f.update("alpha", "--build", devArchive)
		t.Logf("the update said:\n%s", out)
		for _, want := range []string{"alpha: sending " + filepath.Base(devArchive), "which no release vouches for",
			"the machine is set to trust this master's hash", "alpha: back on the new version " + devVersion} {
			if !strings.Contains(out, want) {
				t.Errorf("the update's output lacks %q", want)
			}
		}
		if exit != 0 || strings.Contains(out, "fetching") {
			t.Fatalf("exit %d\n%s", exit, out)
		}
		if got := e2eMarkOf(t, f.bin); got != devVersion {
			t.Errorf("the installed binary is marked %q", got)
		}
		if got := f.reported(t, alpha.id); got != fleetQuote(devVersion) {
			t.Errorf("the master shows alpha on %s", got)
		}
		if st := alpha.state("connected to the master"); !strings.Contains(st, "updated from 3.1.0 to "+devVersion) {
			t.Errorf("tenant child status:\n%s", st)
		}
		if out, exit := f.update("alpha", "--build", devArchive); exit != 0 || !strings.Contains(out, "already runs "+devVersion) {
			t.Errorf("the same build again: exit %d\n%s", exit, out)
		}
	})

	// A machine on the default rule (decision A71) verifies the release
	// itself: a signed release comes back on the new version, and one with
	// no signature bundle is refused before the archive is on the wire.
	step("a machine on the default rule verifies a signed release itself", func(t *testing.T) {
		gamma := f.machine(t, "gamma", true, "")
		if st, _ := gamma.tenant("status"); !strings.Contains(st, "updates:  release") {
			t.Fatalf("gamma status:\n%s", st)
		}
		f.rel.publish("v5.0.1", e2eBuild(t, filepath.Join(builds, "5.0.1"), "5.0.1", ""))
		out, exit := f.update("gamma", "--version", "v5.0.1")
		if exit == 0 || !strings.Contains(out, "carries no signature bundle") {
			t.Errorf("an unsigned release to a default machine: exit %d\n%s", exit, out)
		}
		if parts, _ := filepath.Glob(filepath.Join(childDataDir(gamma.dir), "update", "*")); len(parts) != 0 {
			t.Errorf("the refused release left %v on the machine", parts)
		}
		f.rel.publish("v5.0.0", e2eBuild(t, filepath.Join(builds, "5.0.0"), "5.0.0", ""))
		out, exit = f.update("gamma", "--version", "v5.0.0")
		if exit != 0 || !strings.Contains(out, "verified the release's signature itself") || !strings.Contains(out, "back on the new version 5.0.0") {
			t.Errorf("a signed release to a default machine: exit %d\n%s", exit, out)
		}
		if got := f.reported(t, gamma.id); got != fleetQuote("5.0.0") {
			t.Errorf("gamma reports %s", got)
		}
		// And it takes no build of the operator's own (TEN-552): no release
		// vouches for one. It refuses on the first piece, and the operator
		// is told what would make it take one.
		out, exit = f.update("gamma", "--build", devArchive)
		if exit == 0 || !strings.Contains(out, "the machine refused the update") || !strings.Contains(out, "`tenant child updates master`, run on that machine") {
			t.Errorf("a build to a machine that verifies releases: exit %d\n%s", exit, out)
		}
		if parts, _ := filepath.Glob(filepath.Join(childDataDir(gamma.dir), "update", "*")); len(parts) != 0 {
			t.Errorf("the refused build left %v on the machine", parts)
		}
		if got := f.reported(t, gamma.id); got != fleetQuote("5.0.0") {
			t.Errorf("after a refused build gamma reports %s", got)
		}
	})

	// A machine on the default rule whose Tenant carries the project key
	// (TEN-542), with no cosign on it: a release signed with the key comes
	// back on the new version; one whose signature has one byte changed is
	// refused before the archive is on the wire.
	step("a machine that carries the project key verifies a release with it, with no cosign", func(t *testing.T) {
		delta := f.machine(t, "delta", true, e2eProjectPub)
		f.rel.publish("v6.0.1", e2eBuild(t, filepath.Join(builds, "6.0.1"), "6.0.1", ""))
		out, exit := f.update("delta", "--version", "v6.0.1")
		if exit == 0 || !strings.Contains(out, "project signature does not verify") {
			t.Errorf("a release whose signature has a byte changed: exit %d\n%s", exit, out)
		}
		if parts, _ := filepath.Glob(filepath.Join(childDataDir(delta.dir), "update", "*")); len(parts) != 0 {
			t.Errorf("the refused release left %v on the machine", parts)
		}
		f.rel.publish("v6.0.0", e2eBuild(t, filepath.Join(builds, "6.0.0"), "6.0.0", ""))
		out, exit = f.update("delta", "--version", "v6.0.0")
		if exit != 0 || !strings.Contains(out, "with the project key its own Tenant carries") || !strings.Contains(out, "back on the new version 6.0.0") {
			t.Errorf("a release signed with the project key: exit %d\n%s", exit, out)
		}
		t.Logf("the update said:\n%s", out)
		if got := f.reported(t, delta.id); got != fleetQuote("6.0.0") {
			t.Errorf("delta reports %s", got)
		}
	})

	// TEN-552: the master is killed once the machine holds the whole archive
	// and is applying it, so nobody waits for the machine's word. The master
	// that starts next says so, sends nothing, and records what the machine
	// says on its next link. Which that is depends on how soon the master is
	// back: within the new version's fifteen seconds its trial run reaches
	// it and the machine comes back updated; later, the machine puts back
	// what it had. Either is the machine's own word, and the record has to
	// agree with the version the machine runs. It is the last step: alpha
	// ends on one of two versions.
	step("the master is killed while the machine applies an update, and has its word once both are back", func(t *testing.T) {
		was := e2eMarkOf(t, f.bin)
		f.rel.publish("v7.0.0", e2eBuild(t, filepath.Join(builds, "7.0.0"), "7.0.0", ""))
		cli := h.start("fleet update", e2eSpec{Argv: []string{"fleet", "update", "alpha", "--version", "v7.0.0", "--hub", m.dash, "--config", m.cfg, "--data", m.data}})
		for deadline := time.Now().Add(e2eWait); ; time.Sleep(10 * time.Millisecond) {
			if doc, _, ok := savedDoc(m.data); ok && doc.Machines[alpha.id] != nil && doc.Machines[alpha.id].Stage == fleetUpdApplying && doc.Machines[alpha.id].To == "7.0.0" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the master never saved that alpha is applying\n%s", cli.said())
			}
		}
		m.proc.kill()
		cli.ended("the command line gives up on a master that is gone")
		m.serve()
		f.wire.to(m.fleet)
		var u *hubUpdate
		for deadline := time.Now().Add(e2eWait); ; time.Sleep(100 * time.Millisecond) {
			if u = f.lastUpdate(t, alpha.id); u != nil && (u.Stage == fleetUpdDone || u.Stage == fleetUpdRolledBack) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the restarted master never recorded how alpha's update went: %+v\n%s", u, alpha.proc.said())
			}
		}
		t.Logf("the restarted master's record:\n%s", strings.Join(u.Lines, "\n"))
		said := strings.Join(u.Lines, "\n")
		if u.Running || !strings.Contains(said, "this Tenant stopped while the machine was applying the update") || !strings.HasPrefix(u.Lines[len(u.Lines)-1], "the machine linked again: ") {
			t.Errorf("the record: %+v", u)
		}
		runs := "7.0.0"
		if u.Stage == fleetUpdRolledBack {
			runs = was
		}
		alpha.state("connected to the master")
		if got := e2eMarkOf(t, f.bin); got != runs {
			t.Errorf("the record says %s and the installed binary is marked %q, want %q", u.Stage, got, runs)
		}
		for deadline := time.Now().Add(e2eWait); f.reported(t, alpha.id) != fleetQuote(runs); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the record says %s and the master shows alpha on %s, want %q", u.Stage, f.reported(t, alpha.id), runs)
			}
		}
		// Asked again, the master sends only what the machine does not have.
		out, exit := f.update("alpha", "--version", "v7.0.0")
		if u.Stage == fleetUpdDone && (exit != 0 || !strings.Contains(out, "already runs 7.0.0")) {
			t.Errorf("the update again, to a machine that has it: exit %d\n%s", exit, out)
		}
		if u.Stage == fleetUpdRolledBack && (exit != 0 || !strings.Contains(out, "back on the new version 7.0.0")) {
			t.Errorf("the update again, to a machine that rolled back: exit %d\n%s", exit, out)
		}
	})
}

// lastUpdate is the machine's last update as the master's API shows it; nil
// when it has none.
func (f *e2eUpdateFleet) lastUpdate(t *testing.T, id string) *hubUpdate {
	t.Helper()
	var v apiFleet
	if code, body := f.m.api(http.MethodGet, "/api/fleet", nil, &v); code != http.StatusOK {
		t.Fatalf("/api/fleet: %d %s", code, body)
	}
	for _, m := range v.Machines {
		if m.ID == id {
			return m.Update
		}
	}
	return nil
}

// e2eProjectKey is the throwaway project key the v6 releases are signed
// with, and e2eProjectPub its public text, which delta's binary carries.
var e2eProjectKey, e2eProjectPub = projectKey(6)
