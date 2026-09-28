package archtest

import (
	"reflect"
	"strings"
	"testing"
)

// No new stderr writes or log package calls where serve and the TUI run.
func TestStrayOutputRatchet(t *testing.T) {
	root, _ := moduleRoot(t)
	files, err := parseTree(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range strayRatchet(strayAllowed, strayCalls(files)) {
		t.Error(v)
	}
}

// Every http.Server gets the shared limits and an error log that reaches the
// log book.
func TestEveryHTTPServerIsConfigured(t *testing.T) {
	root, mod := moduleRoot(t)
	files, err := parseTree(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range unconfiguredServers(files, mod) {
		t.Errorf("%s builds an http.Server without httplimits.Configure: its errors would go to stderr", f)
	}
	fixture, err := parseTree("testdata/stray")
	if err != nil {
		t.Fatal(err)
	}
	if got := unconfiguredServers(fixture, "tenant"); !reflect.DeepEqual(got, []string{"internal/p/srv.go"}) {
		t.Errorf("unconfiguredServers(fixture) = %v, want [internal/p/srv.go]", got)
	}
}

func TestStrayCallsOnAFixture(t *testing.T) {
	files, err := parseTree("testdata/stray")
	if err != nil {
		t.Fatal(err)
	}
	got := strayCalls(files)
	if want := map[string]int{"internal/p/p.go": 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("strayCalls = %v, want %v", got, want)
	}
	for _, c := range []struct {
		allowed map[string]int
		want    string
	}{
		{map[string]int{"internal/p/p.go": 3}, ""},
		{map[string]int{"internal/p/p.go": 2}, "3 times, more than its 2"},
		{nil, "3 times, more than its 0"},
		{map[string]int{"internal/p/p.go": 4}, "from 4 to 3"},
		{map[string]int{"internal/p/p.go": 3, "gone.go": 1}, "from 1 to 0"},
	} {
		msg := strings.Join(strayRatchet(c.allowed, got), "\n")
		if c.want == "" && msg != "" || !strings.Contains(msg, c.want) {
			t.Errorf("strayRatchet(%v) = %q, want %q", c.allowed, msg, c.want)
		}
	}
}
