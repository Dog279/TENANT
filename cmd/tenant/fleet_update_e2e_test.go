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
		for _, name := range []string{selfupdate.AssetName(selfupdate.VersionOf(tag), runtime.GOOS, runtime.GOARCH), "checksums.txt", "checksums.txt.sigstore.json"} {
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
	f.alpha = f.machine(t, "alpha")
	f.bin = f.alpha.host.bin
	return f
}

// machine enrolls, confirms and runs a machine from its own copy of this
// binary, marked 1.0.0. It takes its master's hash (`--updates master`),
// as a development build must; release is a machine on the default rule
// whose binary carries the release identity test/tenant.
func (f *e2eUpdateFleet) machine(t *testing.T, name string, release ...bool) *e2eChild {
	t.Helper()
	host := *f.h
	host.bin = e2eBuild(t, f.h.join("bin-"+name), "1.0.0", "")
	flags := []string{"--updates", "master"}
	env := map[string]string{"TENANT_E2E_CHILD_HEALTH": "15s"}
	if len(release) > 0 && release[0] {
		flags, env["TENANT_E2E_CHILD_RELEASE"] = nil, "test/tenant"
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
	step("a command is running on the machine when the update starts", func(t *testing.T) {
		startFile, endFile = h.join("bg-start"), h.join("bg-end")
		res, cards := m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.slow(startFile, 45, endFile, "never-printed"), "background": true}, "approve")
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
		for _, want := range []string{"alpha: fetching v2.0.0 for " + runtime.GOOS + "/" + runtime.GOARCH, "alpha: fetched tenant_2.0.0_", "verified checksum only",
			"the machine checked its SHA-256 and is applying it. Its background commands end at the restart",
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
		time.Sleep(time.Until(started.Add(50 * time.Second)))
		if _, err := os.Stat(endFile); err == nil {
			t.Error("the background command outlived the update")
		}
	})

	step("an offline machine is refused, and --all goes on past it", func(t *testing.T) {
		beta := f.machine(t, "beta")
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

	// A machine on the default rule (decision A71) verifies the release
	// itself: a signed release comes back on the new version, and one with
	// no signature bundle is refused before the archive is on the wire.
	step("a machine on the default rule verifies a signed release itself", func(t *testing.T) {
		gamma := f.machine(t, "gamma", true)
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
	})
}
