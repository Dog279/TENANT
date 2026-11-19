package fleet

import (
	"encoding/json"
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
