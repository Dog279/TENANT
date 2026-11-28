package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The hello says which processor the machine has and what became of its last
// update. An older child says neither and is read as before; an older
// master's reading of a newer child's hello is the plain decode of
// TestWireShapesThatNeverChange. What a child says is bounded like the rest
// of the hello: an architecture no release is built for is not kept, and a
// report that is not one is dropped.
func TestHelloArchAndUpdate(t *testing.T) {
	r := newRig(t)
	k := r.kid(t, "box", StateConfirmed)
	report := &UpdateReport{From: "1.2.2", To: "1.2.3", State: UpdateRolledBack, Reason: "it\x1b[31m exited\nat once", At: time.Unix(1, 0).UTC()}
	k.c.cfg.Hello = func() *UpdateReport { return report }
	k.start()
	m := r.awaitOnline(t, k.mach.ID, true)
	if knownArch[runtime.GOARCH] && m.Arch != runtime.GOARCH {
		t.Errorf("arch = %q, want %q", m.Arch, runtime.GOARCH)
	}
	if m.Update == nil || m.Update.State != UpdateRolledBack || m.Update.To != "1.2.3" || m.Update.Reason != "it [31m exited at once" {
		t.Errorf("update = %+v", m.Update)
	}
	k.stop()
	r.awaitOnline(t, k.mach.ID, false)
	if m := r.status(t, k.mach.ID); m.Arch != "" || m.Update != nil {
		t.Errorf("an offline machine still shows the arch %q and update %+v", m.Arch, m.Update)
	}

	// What the master keeps of each hello a child could send.
	for _, tc := range []struct {
		name, hello string
		arch        string
		state       string // "" for no report
	}{
		{"an older child", `{"link":1,"os":"linux","ceiling":"full"}`, "", ""},
		{"arm64", `{"link":1,"os":"linux","ceiling":"full","arch":"arm64"}`, "arm64", ""},
		{"an arch no release is built for", `{"link":1,"os":"linux","ceiling":"full","arch":"sparc64"}`, "", ""},
		{"an arch with a path in it", `{"link":1,"os":"linux","ceiling":"full","arch":"../amd64"}`, "", ""},
		{"done", `{"link":1,"os":"linux","ceiling":"full","update":{"from":"1","to":"2","state":"done","at":"2026-11-19T00:00:00Z"}}`, "", UpdateDone},
		{"a state that is not one", `{"link":1,"os":"linux","ceiling":"full","update":{"from":"1","to":"2","state":"pwned"}}`, "", ""},
		{"no version it went to", `{"link":1,"os":"linux","ceiling":"full","update":{"from":"1","state":"done"}}`, "", ""},
		{"a version that is not text", `{"link":1,"os":"linux","ceiling":"full","update":{"from":"1","to":"2‮","state":"done"}}`, "", ""},
		{"a long version", `{"link":1,"os":"linux","ceiling":"full","update":{"from":"1","to":"` + strings.Repeat("9", maxReportedLen+1) + `","state":"done"}}`, "", ""},
		{"null", `{"link":1,"os":"linux","ceiling":"full","update":null,"arch":null}`, "", ""},
	} {
		var res helloResult
		if err := json.Unmarshal([]byte(tc.hello), &res); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		l := &link{}
		l.update = res.Update.check()
		if knownArch[res.Arch] {
			l.arch = res.Arch
		}
		state := ""
		if l.update != nil {
			state = l.update.State
		}
		if l.arch != tc.arch || state != tc.state {
			t.Errorf("%s: kept arch %q and update %+v", tc.name, l.arch, l.update)
		}
	}
	// A reason is cut to its bound.
	long := (&UpdateReport{To: "2", State: UpdateRolledBack, Reason: strings.Repeat("é", maxUpdateReason)}).check()
	if long == nil || len(long.Reason) > maxUpdateReason {
		t.Errorf("a long reason is kept at %d bytes", len(long.Reason))
	}
}

// A machine that says it is applying an update restarts into the new version,
// and a call that reached it in between would end with the process that had
// it. So the master keeps such a call back, unsent, until the machine says
// how the update went (TEN-552): the hello of the trial run does not end the
// wait, nor does word of another version; done does, rolled back does, and so
// does the hold's limit. What is not a tool call is not kept back.
func TestCallsWaitOutAnUpdate(t *testing.T) {
	update := Update{Version: "2.0.0", SHA256: fuzzSum, Size: 4, Data: []byte("abcd")}
	type answer struct {
		res Result
		err error
	}
	// applying brings a machine up and has it take the update whole.
	applying := func(t *testing.T, r *rig) *kid {
		t.Helper()
		k := r.up(t, "box")
		if ack, err := r.mgr.Update(ctxFor(t), k.mach.ID, update); err != nil || !ack.Applying {
			t.Fatalf("update = %+v, %v", ack, err)
		}
		return k
	}
	// calling makes a call in the background.
	calling := func(t *testing.T, r *rig, k *kid) chan answer {
		t.Helper()
		got := make(chan answer, 1)
		go func() {
			res, err := r.call(t, k.mach.ID, "echo", `"after"`)
			got <- answer{res, err}
		}()
		return got
	}
	// kept fails the test unless the call is still waiting, and was not sent.
	kept := func(t *testing.T, k *kid, got chan answer, when string) {
		t.Helper()
		select {
		case a := <-got:
			t.Fatalf("%s: the call was not kept back: %+v, %v", when, a.res, a.err)
		case call := <-k.tools.started:
			t.Fatalf("%s: the call was sent to the machine: %+v", when, call)
		case <-time.After(3 * holdPoll):
		}
	}
	// made fails the test unless the call has now been made, and answered.
	made := func(t *testing.T, got chan answer, when string) {
		t.Helper()
		select {
		case a := <-got:
			if a.err != nil || a.res.Text != `"after"` {
				t.Fatalf("%s: the call = %+v, %v", when, a.res, a.err)
			}
		case <-time.After(waitLong):
			t.Fatalf("%s: the call is still kept back", when)
		}
	}
	// restart is the machine's next process: a new link, with a hello that
	// says report.
	restart := func(t *testing.T, r *rig, k *kid, report UpdateReport) {
		t.Helper()
		old := r.link(t, k.mach.ID)
		k.stop()
		k.client().cfg.Hello = func() *UpdateReport { return &report }
		k.start()
		r.awaitNewLink(t, k.mach.ID, old)
	}
	long := func(m *Manager) { m.holdFor = time.Hour }

	t.Run("until the machine says the update is done", func(t *testing.T) {
		r := newRig(t, long)
		k := applying(t, r)
		got := calling(t, r, k)
		kept(t, k, got, "while it applies the update")
		// Ping and the rest of an update are not tool calls.
		if _, err := r.mgr.Ping(ctxFor(t), k.mach.ID); err != nil {
			t.Errorf("ping to a machine applying an update: %v", err)
		}
		if _, err := r.mgr.Update(ctxFor(t), k.mach.ID, update); err != nil {
			t.Errorf("a piece of the update again: %v", err)
		}
		restart(t, r, k, UpdateReport{From: "1.2.3", To: "2.0.0", State: UpdateTrial})
		kept(t, k, got, "during the trial run")
		restart(t, r, k, UpdateReport{From: "1.0.0", To: "1.2.3", State: UpdateDone})
		kept(t, k, got, "after word of another update")
		restart(t, r, k, UpdateReport{From: "1.2.3", To: "2.0.0", State: UpdateDone})
		made(t, got, "once the machine said done")
		r.echo(t, k.mach.ID) // and nothing is kept back after it
	})
	t.Run("until the machine says it rolled back", func(t *testing.T) {
		r := newRig(t, long)
		k := applying(t, r)
		tools := make(chan error, 1)
		go func() {
			_, err := r.mgr.Tools(ctxFor(t), k.mach.ID)
			tools <- err
		}()
		got := calling(t, r, k)
		kept(t, k, got, "while it applies the update")
		select {
		case err := <-tools:
			t.Fatalf("the machine's tools were asked for while it applies an update: %v", err)
		default:
		}
		restart(t, r, k, UpdateReport{From: "1.2.3", To: "2.0.0", State: UpdateRolledBack, Reason: "it did not start"})
		made(t, got, "once the machine said rolled back")
		if err := <-tools; err != nil {
			t.Errorf("tools after the rollback: %v", err)
		}
	})
	t.Run("no longer than the limit", func(t *testing.T) {
		r := newRig(t, func(m *Manager) { m.holdFor = 5 * holdPoll })
		k := applying(t, r) // and it never says how the update went
		start := time.Now()
		made(t, calling(t, r, k), "after the limit")
		if took := time.Since(start); took < 4*holdPoll {
			t.Errorf("the call was made after %s, before the limit", took)
		}
	})
	t.Run("not past the caller's own deadline", func(t *testing.T) {
		r := newRig(t, long)
		k := applying(t, r)
		ctx, cancel := context.WithTimeout(context.Background(), 3*holdPoll)
		defer cancel()
		_, err := r.mgr.Call(ctx, k.mach.ID, Call{Tool: "echo", Arguments: json.RawMessage(`1`)})
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "was not sent") || !strings.Contains(err.Error(), "2.0.0") {
			t.Errorf("a call whose time ran out while it was kept back = %v", err)
		}
		if len(k.tools.started) != 0 {
			t.Error("the call was sent all the same")
		}
	})
	t.Run("not for a machine that is paused meanwhile", func(t *testing.T) {
		r := newRig(t, long)
		k := applying(t, r)
		got := calling(t, r, k)
		kept(t, k, got, "while it applies the update")
		if err := r.store.Pause(k.mach.ID); err != nil {
			t.Fatal(err)
		}
		select {
		case a := <-got:
			if !errors.Is(a.err, ErrPaused) {
				t.Errorf("the call to a machine paused while it was kept back = %+v, %v", a.res, a.err)
			}
		case <-time.After(waitLong):
			t.Fatal("the call is still kept back for a paused machine")
		}
	})
}

// Served is the master's side of speaks, for doctor.
func TestServed(t *testing.T) {
	for link, want := range map[int]bool{0: false, -1: false, LinkVersion: true, LinkVersion + 5: true} {
		if Served(link) != want {
			t.Errorf("Served(%d) = %v", link, !want)
		}
	}
}

// child-update.json reads back what was written, and is not there before.
func TestChildUpdateFile(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := ReadChildUpdate(dir); ok || err != nil {
		t.Fatalf("no file: ok %v, %v", ok, err)
	}
	want := ChildUpdate{UpdateReport: UpdateReport{From: "1", To: "2", State: UpdateTrial, At: time.Unix(5, 0).UTC()},
		Step: StepCommit, SHA256: fuzzSum, Deadline: time.Unix(65, 0).UTC()}
	if err := WriteChildUpdate(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadChildUpdate(dir)
	if !ok || err != nil || got != want {
		t.Errorf("read back %+v, %v, %v", got, ok, err)
	}
	if err := writePrivate(ChildUpdatePath(dir), []byte("{")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadChildUpdate(dir); ok || err == nil {
		t.Error("a torn file reads as an update")
	}
	if err := writePrivate(ChildUpdatePath(dir), make([]byte, maxChildBytes+1)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadChildUpdate(dir); ok || err == nil {
		t.Error("an oversized file reads as an update")
	}
}

// FuzzHelloUpdate: whatever a child puts in its hello, the master keeps an
// architecture only from its list, and a report only when it is bounded,
// printable and in a known state.
func FuzzHelloUpdate(f *testing.F) {
	for _, seed := range []string{
		`{"link":1,"os":"linux","ceiling":"full","arch":"amd64","update":{"from":"1.2.3","to":"1.3.0","state":"done","at":"2026-11-19T00:00:00Z"}}`,
		`{"link":1,"update":{"from":"1","to":"2","state":"rolled_back","reason":"it exited with code 1"}}`,
		`{"link":1,"update":{"from":"1","to":"2","state":"trial"}}`,
		`{"link":1,"arch":"amd64\u0000"}`,
		`{"link":1,"update":{"to":"‮","state":"done","reason":"\u001b[2J"}}`,
		`{"link":1,"update":[]}`,
		`{"link":1,"update":{"to":"2","state":"done","reason":"` + strings.Repeat("\u00e9", 600) + `"}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var res helloResult
		if json.Unmarshal(raw, &res) != nil {
			return
		}
		r := res.Update.check()
		if r == nil {
			return
		}
		switch r.State {
		case UpdateTrial, UpdateDone, UpdateRolledBack:
		default:
			t.Fatalf("kept state %q", r.State)
		}
		if r.To == "" || len(r.To) > maxReportedLen || len(r.From) > maxReportedLen || !printable(r.To) || !printable(r.From) {
			t.Fatalf("kept versions %q %q", r.From, r.To)
		}
		if len(r.Reason) > maxUpdateReason || !printable(r.Reason) {
			t.Fatalf("kept reason %q", r.Reason)
		}
	})
}

// The release's material rides on a piece of an update (decision A71): an
// archive's name, the checksums and the bundle, each bounded. A piece with
// more, or a name that is a path, is not a piece of an update.
func TestUpdateReleaseMaterialIsBounded(t *testing.T) {
	ok := Update{Version: "3.0.0", SHA256: strings.Repeat("ab", 32), Size: 10, Asset: "tenant_3.0.0_linux_x86_64.tar.gz",
		Checksums: make([]byte, MaxReleaseChecksums), Bundle: make([]byte, MaxReleaseBundle), Signature: make([]byte, MaxReleaseSignature)}
	if err := ok.validate(); err != nil {
		t.Fatalf("material at its bounds: %v", err)
	}
	for name, change := range map[string]func(*Update){
		"checksums too large":       func(u *Update) { u.Checksums = make([]byte, MaxReleaseChecksums+1) },
		"a bundle too large":        func(u *Update) { u.Bundle = make([]byte, MaxReleaseBundle+1) },
		"a signature too large":     func(u *Update) { u.Signature = make([]byte, MaxReleaseSignature+1) },
		"an asset that is a path":   func(u *Update) { u.Asset = "../tenant_3.0.0_linux_x86_64.tar.gz" },
		"an asset with a backslash": func(u *Update) { u.Asset = `x\tenant.zip` },
		"a hidden asset":            func(u *Update) { u.Asset = ".tenant.zip" },
		"an asset that is long":     func(u *Update) { u.Asset = strings.Repeat("a", maxAssetLen+1) },
		"an asset with a control":   func(u *Update) { u.Asset = "tenant\x1b[2J.zip" },
	} {
		u := ok
		change(&u)
		if u.validate() == nil {
			t.Errorf("%s: valid", name)
		}
	}
}

// FuzzUpdateParams: the params of fleet/update, as a child reads them from
// its master. What validates is bounded: a piece of at most MaxUpdateChunk
// inside the archive, material within its bounds, an asset name that is a
// bare file name.
func FuzzUpdateParams(f *testing.F) {
	sha := strings.Repeat("ab", 32)
	for _, seed := range []string{
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"offset":0,"data":null,"asset":"tenant_3.0.0_linux_x86_64.tar.gz","checksums":"YWJj","bundle":"eA=="}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"offset":0,"data":"AAAA"}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"asset":"../../etc/passwd"}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"asset":"C:tenant.zip"}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"asset":"tenant\u0000.zip"}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"checksums":"` + strings.Repeat("A", 90000) + `"}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"bundle":12}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"asset":"tenant_3.0.0_linux_x86_64.tar.gz","checksums":"YWJj","signature":"` + strings.Repeat("QUFB", 30) + `"}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"signature":"` + strings.Repeat("A", 200) + `"}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"signature":"not base64!"}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"signature":["x"],"later":{"field":1}}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":-1,"offset":5}`,
		`{"version":"3.0.0","sha256":"` + sha + `","size":10,"asset":"‮zip.exe"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var u Update
		if json.Unmarshal(raw, &u) != nil || u.validate() != nil {
			return
		}
		if len(u.Checksums) > MaxReleaseChecksums || len(u.Bundle) > MaxReleaseBundle || len(u.Signature) > MaxReleaseSignature || len(u.Data) > MaxUpdateChunk ||
			u.Offset < 0 || u.Size < 1 || u.Offset+int64(len(u.Data)) > u.Size {
			t.Fatalf("validated out of bounds: asset %q, %d/%d bytes of material", u.Asset, len(u.Checksums), len(u.Bundle))
		}
		if u.Asset != "" && (len(u.Asset) > maxAssetLen || !printable(u.Asset) || strings.ContainsAny(u.Asset, `/\: `) ||
			strings.HasPrefix(u.Asset, ".") || filepath.Base(u.Asset) != u.Asset) {
			t.Fatalf("validated asset %q", u.Asset)
		}
	})
}
