package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// archiveOf packs members (name -> bytes) as a zip or a gzip'd tar.
func archiveOf(t *testing.T, zipped bool, members map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if zipped {
		zw := zip.NewWriter(&buf)
		for name, b := range members {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(b)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, b := range members {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(b)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// childUpdaterRig is a child's updater on temp dirs: the child's directory,
// and the folder of a binary that is never run.
func childUpdaterRig(t *testing.T) (*childUpdater, string) {
	t.Helper()
	root := realTemp(t)
	dir := filepath.Join(root, "child")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "bin", exeName())
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	// These tests are about taking and staging an archive on the hash the
	// master names; childrelease_test.go has the default rule.
	rigChild(t, dir, fleet.UpdatesMaster)
	return newChildUpdater(dir, exe, quietLog), dir
}

// rigChild writes a child.json in dir with the update rule rule.
func rigChild(t *testing.T, dir string, rule fleet.UpdateRule) {
	t.Helper()
	if err := fleet.SaveChild(fleet.ChildPath(dir), fleet.Child{ID: "m0123456789abcdef", Name: "box", MasterAddresses: []string{"127.0.0.1:9"},
		MasterFingerprint: strings.Repeat("ab", 32), Sandbox: "host", Ceiling: fleet.CeilingFull, Updates: rule}); err != nil {
		t.Fatal(err)
	}
}

// helperCommands makes childCommand start this test binary as a helper
// process, as a release binary would be started, until the test ends.
func helperCommands(t *testing.T) {
	t.Helper()
	prev := childCommand
	e2eUpdateSeams(e2eSpec{})
	t.Cleanup(func() { childCommand = prev })
}

// send gives the updater the archive in pieces of n bytes and returns the
// last answer.
func send(t *testing.T, u *childUpdater, ver string, archive []byte, n int) (fleet.UpdateAck, error) {
	t.Helper()
	p := fleet.Update{Version: ver, SHA256: shaOf(archive), Size: int64(len(archive))}
	var ack fleet.UpdateAck
	var err error
	for off := 0; off < len(archive); off += n {
		p.Offset, p.Data = int64(off), archive[off:min(len(archive), off+n)]
		if ack, err = u.piece(context.Background(), p); err != nil {
			return ack, err
		}
	}
	return ack, nil
}

func noStage(t *testing.T, u *childUpdater, what string) {
	t.Helper()
	if _, err := os.Stat(childStageDir(u.exe)); err == nil {
		t.Errorf("%s: something was unpacked beside the binary", what)
	}
	select {
	case s := <-u.ready:
		t.Errorf("%s: an update was handed on to be applied: %+v", what, s)
	default:
	}
	if u.applying != nil {
		t.Errorf("%s: the updater says it is applying", what)
	}
}

// What a child is sent is untrusted until its hash checks, and nothing is
// touched before then; an archive that is whole and checked is still refused
// when it is not a Tenant that runs here and says the version it was sent as.
func TestChildUpdateRefusals(t *testing.T) {
	noSigning(t) // not about the macOS signing step (see noSigning)
	ctx := context.Background()
	member, other := exeName(), "tenant"
	if runtime.GOOS != "windows" {
		other = "tenant.exe"
	}
	zipped := runtime.GOOS == "windows"

	t.Run("a wrong SHA-256", func(t *testing.T) {
		u, dir := childUpdaterRig(t)
		archive := archiveOf(t, zipped, map[string][]byte{member: []byte("bin")})
		p := fleet.Update{Version: "2.0.0", SHA256: shaOf([]byte("another archive")), Size: int64(len(archive)), Data: archive}
		if _, err := u.piece(ctx, p); err == nil || !strings.Contains(err.Error(), "does not hash to the SHA-256 it was sent with") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("a wrong hash: %v", err)
		}
		if parts, _ := filepath.Glob(filepath.Join(childDataDir(dir), "update", "*")); len(parts) != 0 {
			t.Errorf("the damaged archive was kept: %v", parts)
		}
		noStage(t, u, "a wrong hash")
	})

	t.Run("a truncated transfer, and one that resumes", func(t *testing.T) {
		u, dir := childUpdaterRig(t)
		archive := bytes.Repeat([]byte("x"), 100)
		p := fleet.Update{Version: "2.0.0", SHA256: shaOf(archive), Size: 100, Data: archive[:40]}
		if ack, err := u.piece(ctx, p); err != nil || ack != (fleet.UpdateAck{Received: 40, Rule: "master"}) {
			t.Errorf("the first piece: %+v, %v", ack, err)
		}
		noStage(t, u, "a truncated transfer")
		// A piece out of order writes nothing; the answer says where to go on.
		p.Offset, p.Data = 70, archive[70:]
		if ack, err := u.piece(ctx, p); err != nil || ack.Received != 40 {
			t.Errorf("a piece out of order: %+v, %v", ack, err)
		}
		// The link dropped and came back (or the child restarted): asked, it
		// says what it holds.
		p.Offset, p.Data = 0, nil
		if ack, err := (newChildUpdater(dir, u.exe, quietLog)).piece(ctx, p); err != nil || ack.Received != 40 {
			t.Errorf("asked after a restart: %+v, %v", ack, err)
		}
		// Another transfer starts: the old pieces go.
		q := fleet.Update{Version: "3.0.0", SHA256: shaOf([]byte("y")), Size: 1}
		if _, err := u.piece(ctx, q); err != nil {
			t.Fatal(err)
		}
		if parts, _ := filepath.Glob(filepath.Join(childDataDir(dir), "update", "*")); len(parts) != 0 {
			t.Errorf("an older transfer's pieces were kept: %v", parts)
		}
	})

	for _, tc := range []struct {
		name    string
		archive []byte
		want    string
	}{
		{"not an archive", []byte("MZ this is no archive at all"), "neither a zip nor a gzip'd tar"},
		{"an archive for another OS", archiveOf(t, !zipped, map[string][]byte{other: []byte("bin")}), "not a Tenant for this machine (" + runtime.GOOS + "/" + runtime.GOARCH + ")"},
		{"an archive with no Tenant in it", archiveOf(t, zipped, map[string][]byte{"README.md": []byte("hi")}), "no entry named " + member},
		{"an archive that climbs out", archiveOf(t, zipped, map[string][]byte{"../" + member: []byte("bin")}), "escaping name"},
		{"a binary that does not run here (another architecture)", archiveOf(t, zipped, map[string][]byte{member: []byte("\x7fELF but not really; MZ neither")}), "does not run on this machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := childUpdaterRig(t)
			if _, err := send(t, u, "2.0.0", tc.archive, 1<<20); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Errorf("%v, want %q", err, tc.want)
			}
			noStage(t, u, tc.name)
		})
	}

	if testing.Short() {
		return
	}
	helperCommands(t)
	build := e2eBuild(t, filepath.Join(realTemp(t), "b"), "3.0.0", "")
	raw, err := os.ReadFile(build)
	if err != nil {
		t.Fatal(err)
	}
	archive := archiveOf(t, zipped, map[string][]byte{member: raw})

	t.Run("a binary that says it is another version", func(t *testing.T) {
		u, _ := childUpdaterRig(t)
		if _, err := send(t, u, "2.0.0", archive, fleet.MaxUpdateChunk); err == nil || !strings.Contains(err.Error(), `says it is "tenant 3.0.0`) {
			t.Errorf("%v", err)
		}
		noStage(t, u, "another version")
	})

	t.Run("a whole, checked archive of a Tenant that runs is staged", func(t *testing.T) {
		u, _ := childUpdaterRig(t)
		ack, err := send(t, u, "3.0.0", archive, fleet.MaxUpdateChunk)
		if err != nil || ack != (fleet.UpdateAck{Received: int64(len(archive)), Applying: true, Rule: "master"}) {
			t.Fatalf("%+v, %v", ack, err)
		}
		s := <-u.ready
		if s.to != "3.0.0" || s.sha != shaOf(archive) || filepath.Dir(s.binary) != childStageDir(u.exe) || e2eMarkOf(t, s.binary) != "3.0.0" {
			t.Errorf("staged %+v", s)
		}
		// The master asks again: the same archive is being applied; another
		// one is not taken.
		p := fleet.Update{Version: "3.0.0", SHA256: shaOf(archive), Size: int64(len(archive))}
		if ack, err := u.piece(ctx, p); err != nil || !ack.Applying {
			t.Errorf("asked again: %+v, %v", ack, err)
		}
		p.SHA256 = shaOf([]byte("other"))
		if _, err := u.piece(ctx, p); err == nil || !strings.Contains(err.Error(), "already applying") {
			t.Errorf("another archive while applying: %v", err)
		}
	})
}

// What the next process does with an update the last one left: only the new
// version on its first run confirms one, by its first hello, before the
// deadline; anything else interrupted is recorded as rolled back, and the
// installed binary is the old one.
func TestChildUpdateSettles(t *testing.T) {
	prevV := version.Version
	t.Cleanup(func() { version.Version = prevV })
	version.Version = "2.0.0"
	root := realTemp(t)
	exe := filepath.Join(root, "bin", exeName())
	if err := os.MkdirAll(filepath.Join(childStageDir(exe)), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(t *testing.T, dir string, st fleet.ChildUpdate) {
		t.Helper()
		if err := fleet.WriteChildUpdate(dir, st); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T, dir string) fleet.ChildUpdate {
		t.Helper()
		st, ok, err := fleet.ReadChildUpdate(dir)
		if !ok || err != nil {
			t.Fatalf("no child-update.json: %v", err)
		}
		return st
	}
	in := func(step, from, to string) fleet.ChildUpdate {
		return fleet.ChildUpdate{UpdateReport: fleet.UpdateReport{From: from, To: to, State: fleet.UpdateTrial, At: time.Now().UTC()},
			Step: step, Deadline: time.Now().Add(time.Minute)}
	}

	for _, tc := range []struct {
		name string
		st   fleet.ChildUpdate
		why  string
	}{
		{"killed during the trial", in(fleet.StepTrial, "2.0.0", "3.0.0"), "interrupted during the trial run"},
		{"killed after the trial reached the master", in(fleet.StepProven, "2.0.0", "3.0.0"), "interrupted during the trial run"},
		{"killed between the commit and the swap", in(fleet.StepCommit, "2.0.0", "3.0.0"), "interrupted before the new version first ran"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.st)
			p := settleChildUpdate(dir, exe, quietLog)
			if p.confirm {
				t.Error("an interrupted update is waiting to be confirmed")
			}
			st := read(t, dir)
			if st.State != fleet.UpdateRolledBack || !strings.Contains(st.Reason, tc.why) || st.Step != "" {
				t.Errorf("recorded as %+v", st)
			}
			if r := p.hello(); r == nil || r.State != fleet.UpdateRolledBack || r.To != "3.0.0" {
				t.Errorf("the hello says %+v", r)
			}
			if line := childUpdateLine(dir); !strings.Contains(line, "was rolled back") || !strings.Contains(line, tc.why) {
				t.Errorf("status line %q", line)
			}
		})
	}

	t.Run("the new version's first hello confirms it", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, in(fleet.StepCommit, "1.0.0", "2.0.0"))
		if line := childUpdateLine(dir); !strings.Contains(line, "in the middle of an update from 1.0.0 to 2.0.0") || !strings.Contains(line, "goes back to 1.0.0") {
			t.Errorf("status line mid-update: %q", line)
		}
		p := settleChildUpdate(dir, exe, quietLog)
		if !p.confirm {
			t.Fatal("the new version is not waiting to be confirmed")
		}
		if r := p.hello(); r == nil || r.State != fleet.UpdateDone {
			t.Errorf("the hello says %+v", r)
		}
		select {
		case <-p.confirmed:
		default:
			t.Error("the hello did not confirm the update")
		}
		if p.expire() {
			t.Error("the deadline of a confirmed update rolled it back")
		}
		if st := read(t, dir); st.State != fleet.UpdateDone || st.Step != "" {
			t.Errorf("recorded as %+v", st)
		}
		if line := childUpdateLine(dir); !strings.Contains(line, "updated from 1.0.0 to 2.0.0") {
			t.Errorf("status line %q", line)
		}
	})

	t.Run("past the deadline the new version puts the old one back", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(t.TempDir(), exeName())
		if err := os.WriteFile(bin, []byte("new"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(selfupdate.PrevPath(bin), []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, dir, in(fleet.StepCommit, "1.0.0", "2.0.0"))
		p := settleChildUpdate(dir, bin, quietLog)
		if !p.expire() {
			t.Fatal("the deadline found nothing to roll back")
		}
		if r := p.hello(); r.State == fleet.UpdateDone {
			t.Error("a hello after the deadline confirmed the update")
		}
		if err := p.rollBack(bin); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(bin); string(b) != "old" {
			t.Errorf("the binary after the rollback is %q", b)
		}
		if st := read(t, dir); st.State != fleet.UpdateRolledBack || !strings.Contains(st.Reason, "did not reach the master by") {
			t.Errorf("recorded as %+v", st)
		}
	})

	t.Run("the trial's hello is the proof the old child waits for", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, in(fleet.StepTrial, "1.0.0", "2.0.0"))
		if r := trialHello(dir)(); r == nil || r.State != fleet.UpdateTrial {
			t.Errorf("the trial's hello says %+v", r)
		}
		if st := read(t, dir); st.Step != fleet.StepProven {
			t.Errorf("after the trial's hello: %+v", st)
		}
		// A trial of another version proves nothing.
		write(t, dir, in(fleet.StepTrial, "1.0.0", "9.9.9"))
		trialHello(dir)()
		if st := read(t, dir); st.Step != fleet.StepTrial {
			t.Errorf("a trial of 2.0.0 proved 9.9.9: %+v", st)
		}
	})

	t.Run("nothing to settle", func(t *testing.T) {
		dir := t.TempDir()
		if p := settleChildUpdate(dir, exe, quietLog); p.confirm || p.hello() != nil || childUpdateLine(dir) != "" {
			t.Error("a child that was never updated says something about an update")
		}
	})
}

// --- the master's side, against children that are not what they say ---

// updateRig is a master with a fleet and a release source of the test's own,
// whose one release is a small archive for this system.
type updateRig struct {
	*fleetToolRig
	updates *fleetUpdates
	archive []byte
}

func newUpdateRig(t *testing.T) *updateRig {
	t.Helper()
	r := &updateRig{fleetToolRig: newFleetToolRig(t), updates: newFleetUpdates(t.TempDir())}
	rel := newE2EReleases(t, t.TempDir())
	bin := filepath.Join(t.TempDir(), exeName())
	if err := os.WriteFile(bin, []byte("a tenant"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel.publish("v2.0.0", bin)
	var err error
	if r.archive, err = os.ReadFile(filepath.Join(rel.dir, "v2.0.0", selfupdate.AssetName("2.0.0", runtime.GOOS, runtime.GOARCH))); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TENANT_UPDATE_REPO", "test/tenant")
	prevAPI, prevWait, prevPoll, prevPiece := fleetReleaseAPI, fleetUpdateWait, fleetUpdatePoll, fleetPieceWait
	fleetReleaseAPI, fleetUpdateWait, fleetUpdatePoll, fleetPieceWait = rel.srv.URL, 5*time.Second, 20*time.Millisecond, 3*time.Second
	t.Cleanup(func() {
		fleetReleaseAPI, fleetUpdateWait, fleetUpdatePoll, fleetPieceWait = prevAPI, prevWait, prevPoll, prevPiece
	})
	return r
}

// kidUpdate is how a test's child answers a piece of an update; reconnect,
// when it calls it, makes the child connect again, saying hello.
type kidUpdate func(p fleet.Update, reconnect func(hello func() *fleet.UpdateReport, ver string)) (fleet.UpdateAck, error)

// kid runs a machine whose child reports version 1.0.0 and answers pieces of
// an update with upd. restart starts its child again, as the given version
// saying hello.
func (r *updateRig) kid(name string, upd kidUpdate) (k *fleetKid, restart func(hello func() *fleet.UpdateReport, ver string)) {
	r.t.Helper()
	k = r.enroll(name)
	r.confirm(k)
	var mu sync.Mutex
	var start func(hello func() *fleet.UpdateReport, ver string)
	start = func(hello func() *fleet.UpdateReport, ver string) {
		c, err := fleet.NewClient(fleet.ClientConfig{Dir: k.dir, Tools: &kidTools{}, Version: ver, User: "svc", Log: fleetQuiet, Hello: hello,
			Update: func(_ context.Context, p fleet.Update) (fleet.UpdateAck, error) {
				return upd(p, func(hello func() *fleet.UpdateReport, ver string) {
					time.AfterFunc(100*time.Millisecond, func() {
						mu.Lock()
						stop := k.stop
						mu.Unlock()
						stop()
						start(hello, ver)
					})
				})
			}})
		if err != nil {
			r.t.Error(err)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = c.Run(ctx)
		}()
		var once sync.Once
		mu.Lock()
		k.stop = func() { once.Do(func() { cancel(); <-done }) }
		mu.Unlock()
	}
	start(nil, "1.0.0")
	r.t.Cleanup(func() {
		mu.Lock()
		stop := k.stop
		mu.Unlock()
		stop()
	})
	r.online(k, true)
	return k, func(hello func() *fleet.UpdateReport, ver string) {
		mu.Lock()
		stop := k.stop
		mu.Unlock()
		stop()
		r.online(k, false)
		start(hello, ver)
		r.online(k, true)
	}
}

// result starts an update of k and waits for its end.
func (r *updateRig) result(k *fleetKid, o fleetUpdateOpts) fleetUpdateState {
	r.t.Helper()
	if err := r.updates.Start(r.mgr, k.id, o); err != nil {
		r.t.Fatalf("Start: %v", err)
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(20 * time.Millisecond) {
		if st, ok := r.updates.State(k.id); ok && st.Over() {
			return st
		}
		if time.Now().After(deadline) {
			st, _ := r.updates.State(k.id)
			r.t.Fatalf("the update did not end: %+v", st)
		}
	}
}

// honest takes the pieces as a child does, in order.
func honest(p fleet.Update) fleet.UpdateAck {
	got := p.Offset + int64(len(p.Data))
	return fleet.UpdateAck{Received: got, Applying: got == p.Size}
}

func TestFleetUpdateHostileChildren(t *testing.T) {
	if testing.Short() {
		t.Skip("one case waits out the limit on a piece")
	}
	r := newUpdateRig(t)
	last := func(st fleetUpdateState) string { return st.Lines[len(st.Lines)-1] }
	// One machine, its child answering as each case says: enrollment is
	// limited to a few a minute from one address.
	var mu sync.Mutex
	var behave kidUpdate
	k, restart := r.kid("hostile", func(p fleet.Update, again func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
		mu.Lock()
		b := behave
		mu.Unlock()
		return b(p, again)
	})
	for _, tc := range []struct {
		name  string
		upd   func(fleet.Update, func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error)
		stage string
		want  string
	}{
		{"never takes a piece", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			return fleet.UpdateAck{}, nil
		}, fleetUpdFailed, "stopped taking the archive at 0.0 MB"},
		{"says it holds more than there is", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			return fleet.UpdateAck{Received: p.Size + 5}, nil
		}, fleetUpdFailed, "not about this archive"},
		{"says it is applying what it does not have", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			return fleet.UpdateAck{Received: 1, Applying: true}, nil
		}, fleetUpdFailed, "does not hold all of"},
		{"refuses, in its own words", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			if len(p.Data) == 0 {
				return fleet.UpdateAck{}, nil
			}
			return fleet.UpdateAck{}, errors.New("the archive that arrived does not hash\x1b[2J")
		}, fleetUpdFailed, `the machine refused the update: "fleet: the machine refused fleet/update: fleet: update: the archive that arrived does not hash [2J (-32005)"`},
		{"has it all and does not apply it", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			return fleet.UpdateAck{Received: p.Size}, nil
		}, fleetUpdFailed, "did not start applying it"},
		{"says it is done on a version it does not run", func(p fleet.Update, again func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			ack := honest(p)
			if ack.Applying {
				again(func() *fleet.UpdateReport {
					return &fleet.UpdateReport{From: "1.0.0", To: "2.0.0", State: fleet.UpdateDone}
				}, "1.0.0")
			}
			return ack, nil
		}, fleetUpdFailed, `says the update to 2.0.0 is done, but reports version "1.0.0"`},
		{"rolls back, and says why in words of its own", func(p fleet.Update, again func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			ack := honest(p)
			if ack.Applying {
				again(func() *fleet.UpdateReport {
					return &fleet.UpdateReport{From: "1.0.0", To: "2.0.0", State: fleet.UpdateRolledBack, Reason: "it \x1b]0;pwned\x07 broke"}
				}, "1.0.0")
			}
			return ack, nil
		}, fleetUpdRolledBack, `rolled back to "1.0.0": "it  ]0;pwned  broke"`},
		{"goes quiet after applying", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			return honest(p), nil
		}, fleetUpdFailed, "has not said within 5s"},
		{"comes back on the new version", func(p fleet.Update, again func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			ack := honest(p)
			if ack.Applying {
				again(func() *fleet.UpdateReport {
					return &fleet.UpdateReport{From: "1.0.0", To: "2.0.0", State: fleet.UpdateDone}
				}, "2.0.0")
			}
			return ack, nil
		}, fleetUpdDone, "back on the new version 2.0.0"},
		{"stalls mid-transfer", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
			time.Sleep(10 * time.Second)
			return honest(p), nil
		}, fleetUpdFailed, "stopped answering during the transfer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			behave = tc.upd
			mu.Unlock()
			restart(nil, "1.0.0")
			st := r.result(k, fleetUpdateOpts{version: "v2.0.0"})
			if st.Stage != tc.stage || !strings.Contains(last(st), tc.want) {
				t.Errorf("stage %s, said:\n%s\nwant %s with %q", st.Stage, strings.Join(st.Lines, "\n"), tc.stage, tc.want)
			}
		})
	}
}

// What is refused before a byte is fetched.
func TestFleetUpdateStartRefusals(t *testing.T) {
	r := newUpdateRig(t)
	k, _ := r.kid("web1", func(p fleet.Update, _ func(func() *fleet.UpdateReport, string)) (fleet.UpdateAck, error) {
		return honest(p), nil
	})
	pending := r.enroll("waits")
	for _, tc := range []struct {
		name, id string
		o        fleetUpdateOpts
		want     string
	}{
		{"a version that is not one", k.id, fleetUpdateOpts{version: "2.0.0; rm -rf /"}, "is not a version"},
		{"a version with a path in it", k.id, fleetUpdateOpts{version: "../../x"}, "is not a version"},
		{"an architecture that is not one", k.id, fleetUpdateOpts{arch: "sparc"}, "--arch is amd64 or arm64"},
		{"an unknown machine", "m0000000000000999", fleetUpdateOpts{}, "unknown machine"},
		{"a pending machine", pending.id, fleetUpdateOpts{}, "only a confirmed machine is updated"},
	} {
		if err := r.updates.Start(r.mgr, tc.id, tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	k.stop()
	r.online(k, false)
	if err := r.updates.Start(r.mgr, k.id, fleetUpdateOpts{}); err == nil || !strings.Contains(err.Error(), "is offline") || !strings.Contains(err.Error(), "Nothing is queued") {
		t.Errorf("an offline machine: %v", err)
	}
	if _, ok := r.updates.State(k.id); ok {
		t.Error("a refused update left a state behind")
	}

	// A machine too old to say its processor, a release with no source, and
	// one older than the machine runs: refused before anything is fetched.
	for _, tc := range []struct {
		name string
		st   fleet.MachineStatus
		o    fleetUpdateOpts
		repo string
		want string
	}{
		{"no arch", fleet.MachineStatus{Machine: fleet.Machine{Reported: fleet.Reported{OS: "linux", Version: "1.0.0"}}}, fleetUpdateOpts{version: "v2.0.0"}, "test/tenant", "--arch amd64 or --arch arm64"},
		{"no release source", fleet.MachineStatus{Arch: "amd64", Machine: fleet.Machine{Reported: fleet.Reported{OS: "linux", Version: "1.0.0"}}}, fleetUpdateOpts{}, "", "no release source"},
		{"older", fleet.MachineStatus{Arch: runtime.GOARCH, Machine: fleet.Machine{Reported: fleet.Reported{OS: runtime.GOOS, Version: "3.0.0"}}}, fleetUpdateOpts{version: "v2.0.0"}, "test/tenant", "2.0.0 is older than the \"3.0.0\" it runs"},
		{"not comparable", fleet.MachineStatus{Arch: runtime.GOARCH, Machine: fleet.Machine{Reported: fleet.Reported{OS: runtime.GOOS, Version: "latest-ish"}}}, fleetUpdateOpts{version: "v2.0.0"}, "test/tenant", "not comparable"},
		{"the same", fleet.MachineStatus{Arch: runtime.GOARCH, Machine: fleet.Machine{Reported: fleet.Reported{OS: runtime.GOOS, Version: "2.0.0"}}}, fleetUpdateOpts{version: "v2.0.0"}, "test/tenant", "already runs 2.0.0"},
	} {
		t.Setenv("TENANT_UPDATE_REPO", tc.repo)
		s := &fleetUpdateState{}
		r.updates.run(r.mgr, tc.st, tc.o, s)
		if !s.Over() || !strings.Contains(strings.Join(s.Lines, "\n"), tc.want) {
			t.Errorf("%s: %s %q", tc.name, s.Stage, s.Lines)
		}
		if tc.name == "older" || tc.name == "no arch" {
			if s.Stage != fleetUpdRefused {
				t.Errorf("%s: stage %s", tc.name, s.Stage)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(r.updates.dataDir, "fleet-updates")); err == nil {
		t.Error("a refused update fetched something")
	}
}

// The command line asks the running Tenant, goes on past a machine that is
// refused, and says which were not updated; with no Tenant to ask it sends
// nothing and says why.
func TestFleetUpdateCommandLine(t *testing.T) {
	var mu sync.Mutex
	polls := map[string]int{}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/fleet/update":
			_ = r.ParseForm()
			if r.Form.Get("confirm") != "yes" || r.Form.Get("version") != "v2.0.0" || r.Form.Get("allow_older") != "yes" {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			loc := "/fleet/" + r.Form.Get("machine") + "?msg=ok"
			if r.Form.Get("machine") == "m2" {
				loc = "/fleet/m2?err=Nothing+was+sent%3A+beta+is+offline."
			}
			http.Redirect(w, r, loc, http.StatusSeeOther)
		case "/api/fleet":
			mu.Lock()
			polls["m1"]++
			n := polls["m1"]
			mu.Unlock()
			u := map[string]any{"stage": "sending", "running": true, "lines": []string{"fetched x"}}
			if n > 2 {
				u = map[string]any{"stage": "rolled_back", "running": false, "lines": []string{"fetched x", "sent 1.0 MB", `rolled back to "1.0.0": "it broke"`}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"held_here": true, "machines": []map[string]any{{"id": "m1", "update": u}}})
		}
	}))
	defer hub.Close()
	prev := fleetUpdatePoll
	fleetUpdatePoll = 10 * time.Millisecond
	t.Cleanup(func() { fleetUpdatePoll = prev })
	h := fleetHubClient{base: hub.URL, token: "tok", http: &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	var out bytes.Buffer
	err := h.update(context.Background(), &out, []fleet.Machine{{ID: "m1", Name: "alpha"}, {ID: "m2", Name: "beta"}}, fleetUpdateOpts{version: "v2.0.0", allowOlder: true})
	want := "alpha: fetched x\nalpha: sent 1.0 MB\nalpha: rolled back to \"1.0.0\": \"it broke\"\nalpha: rolled back\nbeta: refused: Nothing was sent: beta is offline.\n"
	if out.String() != want || exitCode(err) != 1 || !strings.Contains(err.Error(), "2 of 2 machines were not updated") {
		t.Errorf("printed:\n%s\nwant:\n%s\nerr %v", out.String(), want, err)
	}
	// No Tenant answers.
	h.base = "http://127.0.0.1:1"
	out.Reset()
	if err := h.update(context.Background(), &out, []fleet.Machine{{ID: "m1", Name: "alpha"}}, fleetUpdateOpts{}); err == nil || !strings.Contains(err.Error(), "nothing was sent") {
		t.Errorf("with nobody to ask: %v", err)
	}
	// The wrong token.
	h.base, h.token = hub.URL, "nope"
	if err := h.update(context.Background(), &out, []fleet.Machine{{ID: "m1", Name: "alpha"}}, fleetUpdateOpts{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("with the wrong token: %v", err)
	}
}

// Doctor on a master: one check per machine that is not revoked, from what the
// running Tenant that holds the fleet says.
func TestFleetDoctorChecks(t *testing.T) {
	prevV := version.Version
	version.Version = "2.0.0"
	t.Cleanup(func() { version.Version = prevV })
	var held atomicBool
	held.set(true)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rows := []map[string]any{
			{"id": "m1", "online": true, "link": "online since x", "link_version": 1, "reported_by_the_machine": map[string]any{"version": fleetQuote("1.0.0")},
				"memory": "not_available", "memory_detail": "known to the process that holds the fleet", "update": map[string]any{"stage": "rolled_back", "running": false, "lines": []string{`rolled back to "1.0.0": "it broke"`}}},
			{"id": "m2", "online": false, "link": "offline, last seen 2026-11-19 08:00"},
			{"id": "m3", "online": true, "link": "online", "link_version": 0, "reported_by_the_machine": map[string]any{"version": fleetQuote("2.0.0")}},
			{"id": "m4", "online": true, "link": "online", "link_version": 1, "reported_by_the_machine": map[string]any{"version": fleetQuote("2.0.0")},
				"memory": "needs_recovery_key", "memory_detail": "key b0b"},
			{"id": "m5", "online": true, "link": "online", "link_version": 1, "reported_by_the_machine": map[string]any{"version": fleetQuote("2.0.0")}, "memory": "in_step"},
			{"id": "m7", "online": true, "link": "online", "link_version": 1, "reported_by_the_machine": map[string]any{"version": fleetQuote("‮9")}},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"held_here": held.get(), "holder": "tenant serve (process 7)", "machines": rows})
	}))
	defer api.Close()
	e := &doctorEnv{c: &commonFlags{cfgDir: t.TempDir(), auditOnly: true}, httpc: api.Client(), lc: &launchConfig{}}
	e.lc.Dashboard.Addr = strings.TrimPrefix(api.URL, "http://")
	confirmed := func(id, name string) fleet.Machine {
		return fleet.Machine{ID: id, Name: name, State: fleet.StateConfirmed}
	}
	machines := []fleet.Machine{confirmed("m1", "behind"), confirmed("m2", "gone"), confirmed("m3", "unserved"), confirmed("m4", "keyless"),
		confirmed("m5", "same"), {ID: "m6", Name: "waits", State: fleet.StatePending}, {ID: "m8", Name: "resting", State: fleet.StatePaused},
		confirmed("m7", "odd"), confirmed("m9", "unlisted")}
	run := func() map[string]checkResult {
		view := &doctorFleetView{}
		out := map[string]checkResult{}
		for _, m := range machines {
			out[m.Name] = checkFleetMachine(context.Background(), e, m, view)
		}
		return out
	}
	got := run()
	for name, want := range map[string]struct {
		status checkStatus
		has    []string
	}{
		"behind":   {statusWarn, []string{"runs 1.0.0, behind this master's 2.0.0", "its last update did not go through", "memory not known: known to the process that holds the fleet"}},
		"gone":     {statusWarn, []string{"not reachable: offline, last seen"}},
		"unserved": {statusFail, []string{"speaks link version 0"}}, // the one this master does not serve today
		"keyless":  {statusFail, []string{"memory needs the recovery key: key b0b"}},
		"same":     {statusOK, []string{"runs 2.0.0, as this master does", "memory in step"}},
		"waits":    {statusWarn, []string{"waiting for confirmation"}},
		"resting":  {statusSkip, []string{"paused"}},
		"odd":      {statusWarn, []string{"not a version this master can compare"}},
		"unlisted": {statusSkip, []string{"does not list it"}},
	} {
		r := got[name]
		for _, h := range want.has {
			if r.Status != want.status || !strings.Contains(r.Detail, h) {
				t.Errorf("%s: %s %q, want %s with %q", name, r.Status.tag(), r.Detail, want.status.tag(), h)
			}
		}
	}
	if got["behind"].Fix != "tenant fleet update behind" || got["unserved"].Fix != "tenant fleet update unserved" ||
		!strings.HasPrefix(got["keyless"].Fix, "tenant fleet recover keyless ") {
		t.Errorf("the fixes: %q, %q, %q", got["behind"].Fix, got["unserved"].Fix, got["keyless"].Fix)
	}
	// A Tenant that does not hold the fleet cannot say who is online.
	held.set(false)
	if r := run()["same"]; r.Status != statusSkip || !strings.Contains(r.Detail, "does not hold the fleet") {
		t.Errorf("held elsewhere: %s %q", r.Status.tag(), r.Detail)
	}
	// Nobody to ask.
	api.Close()
	if r := run()["same"]; r.Status != statusSkip || !strings.Contains(r.Detail, "no running Tenant answers") {
		t.Errorf("no Tenant: %s %q", r.Status.tag(), r.Detail)
	}

	// The checks themselves: one per machine of the fleet that is not
	// revoked, and none on a Tenant with no fleet.
	if n := len(fleetDoctorChecks(t.TempDir())); n != 0 {
		t.Errorf("%d fleet checks on a Tenant with no fleet", n)
	}
	rig := newFleetToolRig(t)
	rig.confirm(rig.enroll("web1"))
	gone := rig.enroll("web2")
	if err := rig.store.Revoke(gone.id); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ck := range fleetDoctorChecks(rig.dir) {
		names = append(names, ck.Name)
	}
	if !slices.Equal(names, []string{"fleet machine web1"}) {
		t.Errorf("the checks: %q", names)
	}
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) get() bool  { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

// FuzzChildUpdatePieces sends a child's updater an archive in pieces of any
// size, at any offset, with the right hash or a wrong one. Whatever arrives,
// nothing is unpacked unless the whole archive hashes to what it was sent
// with, a whole archive with the wrong hash is not kept, and the updater
// never says it holds more than the archive's size.
func FuzzChildUpdatePieces(f *testing.F) {
	f.Add([]byte("PK\x03\x04 not really"), uint16(3), int8(0), true)
	f.Add([]byte{0x1f, 0x8b, 8, 0, 0, 0}, uint16(1), int8(-2), true)
	f.Add(bytes.Repeat([]byte("x"), 300), uint16(64), int8(5), false)
	f.Add([]byte("a"), uint16(0), int8(1), true)
	f.Fuzz(func(t *testing.T, archive []byte, chunk uint16, skew int8, rightHash bool) {
		if len(archive) == 0 || len(archive) > 64<<10 {
			return
		}
		u, dir := childUpdaterRig(t)
		claim := shaOf(archive)
		if !rightHash {
			claim = shaOf(append([]byte{1}, archive...))
		}
		n := max(1, int(chunk)%len(archive)+1)
		p := fleet.Update{Version: "2.0.0", SHA256: claim, Size: int64(len(archive))}
		for off := 0; off < len(archive); off += n {
			at := min(len(archive), max(0, off+int(skew)))
			p.Offset, p.Data = int64(at), archive[at:min(len(archive), at+n)]
			ack, err := u.piece(context.Background(), p)
			if err != nil {
				break
			}
			if ack.Received < 0 || ack.Received > p.Size {
				t.Fatalf("the updater says it holds %d of %d bytes", ack.Received, p.Size)
			}
		}
		select {
		case s := <-u.ready:
			if s.sha != shaOf(archive) || !rightHash {
				t.Fatalf("staged an archive whose hash does not check: %+v", s)
			}
		default:
		}
		if !rightHash {
			parts, _ := filepath.Glob(filepath.Join(childDataDir(dir), "update", "*"))
			for _, part := range parts {
				if fi, err := os.Stat(part); err == nil && fi.Size() == int64(len(archive)) {
					t.Fatalf("a whole archive with the wrong hash was kept: %s", part)
				}
			}
		}
	})
}
