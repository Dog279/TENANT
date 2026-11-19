package fleet

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tenant/internal/secrets"
)

func validChild() Child {
	return Child{
		ID:                "m0123456789abcdef",
		Name:              "box",
		MasterAddresses:   []string{"10.0.0.1:7443", "master.lan:7443"},
		MasterFingerprint: strings.Repeat("ab", 32),
		Sandbox:           "host",
		Ceiling:           CeilingNoExec,
	}
}

func TestChildRoundTrip(t *testing.T) {
	path := ChildPath(filepath.Join(t.TempDir(), "child"))
	want := validChild()
	want.Tools = []string{"os_read_file", "os_list_dir"}
	if err := SaveChild(path, want); err != nil {
		t.Fatal(err)
	}
	got := must(LoadChild(path))(t)
	if got.ID != want.ID || got.Name != want.Name || !slices.Equal(got.MasterAddresses, want.MasterAddresses) ||
		got.MasterFingerprint != want.MasterFingerprint || got.Sandbox != want.Sandbox || got.Ceiling != want.Ceiling ||
		!slices.Equal(got.Tools, want.Tools) {
		t.Errorf("loaded %+v, saved %+v", got, want)
	}
	if report := secrets.Probe(path); !report.Protected {
		t.Errorf("child.json is not owner-only: %+v", report)
	}
	if !got.Serves("os_read_file") || got.Serves("os_exec") {
		t.Error("a tool list is not the only tools served")
	}

	// Without a list the ceiling alone decides, and the key is not written.
	want.Tools = nil
	if err := SaveChild(path, want); err != nil {
		t.Fatal(err)
	}
	if b := must(os.ReadFile(path))(t); strings.Contains(string(b), "tools") {
		t.Errorf("child.json names tools without a list:\n%s", b)
	}
	if got := must(LoadChild(path))(t); got.Tools != nil || !got.Serves("os_exec") {
		t.Errorf("without a list: %+v", got)
	}

	// What is not a child.json does not load, and where none can be written
	// none is.
	huge := ChildPath(t.TempDir())
	if err := os.WriteFile(huge, []byte(strings.Repeat(" ", maxChildBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadChild(huge); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("a child.json over the size limit: %v", err)
	}
	if _, err := LoadChild(t.TempDir()); err == nil {
		t.Error("a directory loaded as child.json")
	}
	nowhere := underAFile(t, "child.json")
	if err := SaveChild(nowhere, want); err == nil {
		t.Error("saved child.json under a file")
	}
	if err := UpdateChild(nowhere, func(*Child) error { return nil }); err == nil {
		t.Error("updated a child.json under a file")
	}

	// A Child that did not come from a valid file serves nothing.
	if (Child{}).Serves("os_read_file") {
		t.Error("the zero Child serves a tool")
	}
	if _, err := LoadChild(filepath.Join(t.TempDir(), "child.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing child.json: %v", err)
	}
}

// child.json is read strictly: anything unexpected stops the child rather
// than leave it running wider than the operator wrote.
func TestParseChildRejects(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	good := `{"version":1,"id":"m0123456789abcdef","name":"box","master_addresses":["10.0.0.1:7443"],"master_fingerprint":"` + fp + `","sandbox":"host","ceiling":"read-only"}`
	if c, err := parseChild([]byte(good)); err != nil || c.Ceiling != CeilingReadOnly {
		t.Fatalf("the valid file: %+v, %v", c, err)
	}
	if c, err := parseChild([]byte(strings.Replace(good, `"ceiling"`, `"tools":null,"ceiling"`, 1))); err != nil || c.Tools != nil {
		t.Errorf("tools: null is the same as no list: %+v, %v", c, err)
	}
	swap := func(old, new string) string {
		out := strings.Replace(good, old, new, 1)
		if out == good {
			t.Fatalf("%q is not in the valid file", old)
		}
		return out
	}
	for name, bad := range map[string]string{
		"empty":                    "",
		"not JSON":                 "ceiling = read-only",
		"an array":                 "[]",
		"null":                     "null",
		"a second value":           good + good,
		"text after the value":     good + " x",
		"a misspelt ceiling key":   swap(`"ceiling"`, `"cieling"`),
		"an unknown key":           swap(`"sandbox"`, `"paused":true,"sandbox"`),
		"no version":               swap(`"version":1,`, ``),
		"a newer version":          swap(`"version":1`, `"version":2`),
		"no id":                    swap(`"id":"m0123456789abcdef",`, ``),
		"an id that is a path":     swap(`m0123456789abcdef`, `../../../../etc`),
		"no name":                  swap(`"name":"box",`, ``),
		"a name with a space":      swap(`"box"`, `"my box"`),
		"no addresses":             swap(`"master_addresses":["10.0.0.1:7443"],`, ``),
		"an empty address list":    swap(`["10.0.0.1:7443"]`, `[]`),
		"an address with no port":  swap(`10.0.0.1:7443`, `10.0.0.1`),
		"a URL for an address":     swap(`10.0.0.1:7443`, `http://10.0.0.1:7443`),
		"no fingerprint":           swap(`"master_fingerprint":"`+fp+`",`, ``),
		"an empty fingerprint":     swap(fp, ``),
		"a short fingerprint":      swap(fp, fp[:62]),
		"an uppercase fingerprint": swap(fp, strings.ToUpper(fp)),
		"a fingerprint with colon": swap(fp, strings.Repeat("ab:", 21)+"a"),
		"no sandbox":               swap(`"sandbox":"host",`, ``),
		"a sandbox with a path":    swap(`"host"`, `"../host"`),
		"no ceiling":               swap(`,"ceiling":"read-only"`, ``),
		"an empty ceiling":         swap(`"read-only"`, `""`),
		"an unknown ceiling":       swap(`"read-only"`, `"readonly"`),
		"a ceiling in capitals":    swap(`"read-only"`, `"Read-Only"`),
		"a ceiling that is a list": swap(`"read-only"`, `["full"]`),
		"an empty tool list":       swap(`"ceiling"`, `"tools":[],"ceiling"`),
		"an empty tool name":       swap(`"ceiling"`, `"tools":[""],"ceiling"`),
		"a tool named twice":       swap(`"ceiling"`, `"tools":["a","a"],"ceiling"`),
		"a tool with a space":      swap(`"ceiling"`, `"tools":["os exec"],"ceiling"`),
		"a tool with a newline":    swap(`"ceiling"`, `"tools":["os\nexec"],"ceiling"`),
		"tools that is a string":   swap(`"ceiling"`, `"tools":"os_exec","ceiling"`),
		"too many tools":           swap(`"ceiling"`, `"tools":["t`+strings.Repeat(`","t`, maxChildTools)+`"],"ceiling"`),
		"too large":                good + strings.Repeat(" ", maxChildBytes),
	} {
		if c, err := parseChild([]byte(bad)); err == nil {
			t.Errorf("%s: accepted as %+v", name, c)
		} else if c.Serves("os_read_file") {
			t.Errorf("%s: refused, and the Child returned with the error serves tools", name)
		}
	}

	// The same checks stand in front of a save.
	c := validChild()
	c.Ceiling = "wide-open"
	path := ChildPath(t.TempDir())
	if err := SaveChild(path, c); err == nil {
		t.Error("saved a child.json with an unknown ceiling")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a refused save wrote the file")
	}
}

// The ceiling is the file on disk, as it is at each check.
func TestChildFileFollowsTheDisk(t *testing.T) {
	path := ChildPath(t.TempDir())
	f := OpenChild(path)
	if c, err := f.Current(); !errors.Is(err, fs.ErrNotExist) || c.Serves("os_read_file") {
		t.Fatalf("before the file exists: %+v, %v", c, err)
	}

	c := validChild()
	c.Ceiling = CeilingFull
	if err := SaveChild(path, c); err != nil {
		t.Fatal(err)
	}
	got := must(f.Current())(t)
	if got.Ceiling != CeilingFull {
		t.Fatalf("Current = %+v", got)
	}
	// What a caller does to its copy is not what the next caller gets.
	got.MasterAddresses[0] = "changed"
	if again := must(f.Current())(t); again.MasterAddresses[0] != c.MasterAddresses[0] {
		t.Error("Current hands out its own slices")
	}

	// The operator lowers the ceiling with an editor: no restart, no signal.
	raw := must(os.ReadFile(path))(t)
	lowered := strings.Replace(string(raw), `"ceiling": "full"`, `"ceiling": "read-only",
  "tools": ["os_read_file"]`, 1)
	writeRaw(t, path, []byte(lowered))
	got = must(f.Current())(t)
	if got.Ceiling != CeilingReadOnly || got.Serves("os_exec") || !got.Serves("os_read_file") {
		t.Errorf("after the edit: %+v", got)
	}

	// A slip of the editor closes the machine until it is put right.
	for name, bad := range map[string]string{
		"a typo":        strings.Replace(lowered, `"read-only"`, `"read-onyl"`, 1),
		"half the file": lowered[:len(lowered)/2],
		"nothing":       "",
	} {
		writeRaw(t, path, []byte(bad))
		if c, err := f.Current(); err == nil || c.Serves("os_read_file") || c.Ceiling != "" {
			t.Errorf("%s: Current = %+v, %v; want an error and a Child that serves nothing", name, c, err)
		}
		if c, err := f.Current(); err == nil || c.Serves("os_read_file") {
			t.Errorf("%s, asked again: %+v, %v", name, c, err)
		}
	}
	writeRaw(t, path, []byte(lowered))
	if got := must(f.Current())(t); got.Ceiling != CeilingReadOnly {
		t.Errorf("after the repair: %+v", got)
	}

	// A file that is taken away is not remembered.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if c, err := f.Current(); !errors.Is(err, fs.ErrNotExist) || c.Serves("os_read_file") {
		t.Errorf("after the file was removed: %+v, %v", c, err)
	}
}

// UpdateChild changes the file as it is now, not as the process remembers it.
func TestUpdateChildKeepsWhatIsOnDisk(t *testing.T) {
	path := ChildPath(t.TempDir())
	c := validChild()
	c.Ceiling = CeilingFull
	if err := SaveChild(path, c); err != nil {
		t.Fatal(err)
	}
	// The operator lowers the ceiling after the running child read the file.
	raw := must(os.ReadFile(path))(t)
	writeRaw(t, path, []byte(strings.Replace(string(raw), `"full"`, `"read-only"`, 1)))

	pushed := []string{"10.9.9.9:7443"}
	if err := UpdateChild(path, func(c *Child) error { c.MasterAddresses = pushed; return nil }); err != nil {
		t.Fatal(err)
	}
	got := must(LoadChild(path))(t)
	if got.Ceiling != CeilingReadOnly || !slices.Equal(got.MasterAddresses, pushed) {
		t.Errorf("after the update: %+v; the ceiling on disk must survive", got)
	}

	// A change that would make the file invalid, or that gives up, is not saved.
	before := must(os.ReadFile(path))(t)
	if err := UpdateChild(path, func(c *Child) error { c.MasterAddresses = nil; return nil }); err == nil {
		t.Error("saved a child.json without a master address")
	}
	stop := errors.New("stop")
	if err := UpdateChild(path, func(c *Child) error { c.Ceiling = CeilingFull; return stop }); !errors.Is(err, stop) {
		t.Errorf("err = %v", err)
	}
	if after := must(os.ReadFile(path))(t); string(after) != string(before) {
		t.Error("a refused update changed the file")
	}
	// And a file that cannot be read is not replaced.
	writeRaw(t, path, []byte("{"))
	if err := UpdateChild(path, func(c *Child) error { return nil }); err == nil {
		t.Error("updated a child.json that does not parse")
	}
	if after := must(os.ReadFile(path))(t); string(after) != "{" {
		t.Error("an unreadable child.json was written over")
	}
}

func TestSystemDir(t *testing.T) {
	for _, tc := range []struct{ goos, programData, want string }{
		{"windows", `C:\ProgramData`, `C:\ProgramData\tenant\child`},
		{"windows", `D:\Data\`, `D:\Data\tenant\child`},
		{"windows", "", `C:\ProgramData\tenant\child`},
		{"linux", "", "/etc/tenant/child"},
		{"darwin", "", "/Library/Application Support/tenant/child"},
		{"freebsd", `C:\ProgramData`, "/etc/tenant/child"},
	} {
		if got := systemDir(tc.goos, tc.programData); got != tc.want {
			t.Errorf("systemDir(%s, %q) = %q, want %q", tc.goos, tc.programData, got, tc.want)
		}
	}
	if SystemDir() == "" || !filepath.IsAbs(SystemDir()) {
		t.Errorf("SystemDir = %q", SystemDir())
	}
	dir := t.TempDir()
	if filepath.Dir(ChildPath(dir)) != dir || filepath.Dir(ChildIdentityPath(dir)) != dir ||
		filepath.Dir(MasterIdentityPath(dir)) != dir || filepath.Dir(StorePath(dir)) != dir {
		t.Error("a fleet file is not directly in its directory")
	}
	names := []string{filepath.Base(ChildPath(dir)), filepath.Base(ChildIdentityPath(dir)), filepath.Base(MasterIdentityPath(dir)), filepath.Base(StorePath(dir))}
	slices.Sort(names)
	if len(slices.Compact(names)) != 4 {
		t.Errorf("a master and a child in one directory would share a file: %v", names)
	}
}
