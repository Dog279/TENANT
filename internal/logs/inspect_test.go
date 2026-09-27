package logs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/runtrace"
)

// A process running on its text files alone leaves a marker that another
// process (doctor) reads, and holds its boot lock while it runs, having no
// boot record. The marker goes once logbook.db is written again, or when the
// hub closes. A marker whose process died reads as gone.
func TestMirrorOnlyMarker(t *testing.T) {
	root := t.TempDir()
	h, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	marks := func() []MirrorOnly {
		t.Helper()
		ms, err := MirrorOnlyProcesses(root)
		if err != nil {
			t.Fatal(err)
		}
		return ms
	}

	// A directory where logbook.db belongs: the store can't open.
	if err := os.Mkdir(filepath.Join(root, "logbook.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.OpenStore(nil); err == nil {
		t.Fatal("the store opened")
	}
	ms := marks()
	if len(ms) != 1 || !ms[0].Running || ms[0].Boot != runtrace.BootID() || ms[0].PID != os.Getpid() ||
		ms[0].Since.IsZero() || !strings.Contains(ms[0].Why, "did not open") {
		t.Fatalf("marks = %+v", ms)
	}

	h.degrade("") // written again
	if ms := marks(); len(ms) != 0 || BootAlive(root, runtrace.BootID()) {
		t.Fatalf("after recovery: marks %+v, lock held %t", ms, BootAlive(root, runtrace.BootID()))
	}
	h.degrade("failing again")
	if ms := marks(); len(ms) != 1 || !ms[0].Running {
		t.Fatalf("marks = %+v", ms)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if ms := marks(); len(ms) != 0 {
		t.Fatalf("after Close: %+v", ms)
	}

	// A process that died marked: its lock file is left, and free.
	const dead = "0123456789abcdef0123456789abcdef"
	for name, body := range map[string]string{dead + ".mirror-only": `{"pid":7,"host":"box","why":"disk full"}`, dead + ".lock": ""} {
		if err := os.WriteFile(filepath.Join(root, "boots", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if ms := marks(); len(ms) != 1 || ms[0].Running || ms[0].Boot != dead || ms[0].PID != 7 || ms[0].Why != "disk full" {
		t.Fatalf("a dead process's marker = %+v", ms)
	}
	if _, err := os.Stat(filepath.Join(root, "boots", dead+".lock")); err != nil {
		t.Errorf("reading the marker removed the dead process's lock file: %v", err)
	}
}
