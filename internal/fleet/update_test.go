package fleet

import (
	"encoding/json"
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
		Step: StepCommit, SHA256: fuzzPrint, Deadline: time.Unix(65, 0).UTC()}
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
