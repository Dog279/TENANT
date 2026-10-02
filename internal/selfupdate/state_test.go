package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update-state.json")
	if _, ok, err := ReadState(path); ok || err != nil {
		t.Fatalf("no marker: ok = %v, err = %v; want false, nil", ok, err)
	}

	want := State{From: "0.2.0", To: "0.3.0", Step: "swap", Started: time.Now().UTC().Truncate(time.Second)}
	if err := WriteState(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadState(path)
	if err != nil || !ok {
		t.Fatalf("ReadState: ok = %v, err = %v", ok, err)
	}
	if got.From != want.From || got.To != want.To || got.Step != want.Step || !got.Started.Equal(want.Started) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode = %o, want 0600", info.Mode().Perm())
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"update-state.json"}) {
		t.Fatalf("dir holds %v; the temp file must be renamed away", names)
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadState(path); err == nil {
		t.Fatal("a torn marker must be an error, not a silent miss")
	}

	for range 2 {
		if err := ClearState(path); err != nil {
			t.Fatalf("ClearState: %v", err)
		}
	}
	if _, ok, _ := ReadState(path); ok {
		t.Fatal("marker still present after ClearState")
	}
}
