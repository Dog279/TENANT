package archtest

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The coverage figure per provider: go test ./internal/archtest -run Coverage -v
// prints it. It fails only when an entry of providers matches no code.
func TestCatalogCoverage(t *testing.T) {
	root, mod := moduleRoot(t)
	files, err := countSites(root, mod)
	if err != nil {
		t.Fatal(err)
	}
	sums := map[string]tally{}
	for f, n := range files {
		p := providerOf(f)
		if p == "" {
			p = "(no provider)"
		}
		s := sums[p]
		s.ID += n.ID
		s.NoID += n.NoID
		sums[p] = s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-14s %8s %8s %9s\n", "provider", "with ID", "without", "coverage")
	for _, p := range providers {
		s, ok := sums[p.Name]
		if !ok {
			t.Errorf("provider %s: none of %q matches a Go file; fix its entry in internal/archtest/coverage.go", p.Name, p.Paths)
			continue
		}
		cov := "-"
		if n := s.ID + s.NoID; n > 0 {
			cov = fmt.Sprintf("%.0f%%", 100*float64(s.ID)/float64(n))
		}
		fmt.Fprintf(&b, "%-14s %8d %8d %9s\n", p.Name, s.ID, s.NoID, cov)
	}
	s := sums["(no provider)"]
	fmt.Fprintf(&b, "%-14s %8d %8d\n", "(no provider)", s.ID, s.NoID)
	t.Log(b.String())
}

// A catalogued package may not add warnings or errors without an event ID.
func TestCatalogRatchet(t *testing.T) {
	root, mod := moduleRoot(t)
	files, err := countSites(root, mod)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range ratchet(catalogued, byPackage(files)) {
		t.Error(v)
	}
}

func TestCountSitesOnAFixture(t *testing.T) {
	files, err := countSites("testdata/coverage", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]tally{"defs/defs.go": {ID: 1}, "use/use.go": {ID: 4, NoID: 6}}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("countSites = %+v, want %+v", files, want)
	}

	pkgs := byPackage(files)
	for _, c := range []struct {
		pkg  string
		max  int
		want string
	}{
		{"use", 6, ""},
		{"defs", 0, ""},
		{"use", 5, "use is catalogued: it has 6 warning or error call sites without an event ID, more than its 5"},
		{"use", 7, "lower its limit in catalogued, internal/archtest/coverage.go, from 7 to 6"},
		{"gone", 0, "gone is marked catalogued in internal/archtest/coverage.go, but there is no such package"},
	} {
		got := strings.Join(ratchet(map[string]int{c.pkg: c.max}, pkgs), "\n")
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("ratchet(%s: %d) = %q, want %q", c.pkg, c.max, got, c.want)
		}
	}
}

// The first matching entry governs a file: a composition-root file glob or
// a specific package before the wider pattern below it.
func TestProviderOf(t *testing.T) {
	for file, want := range map[string]string{
		"cmd/tenant/discordrelay.go":          "discord",
		"cmd/tenant/serve.go":                 "system",
		"internal/plugins/discord/gateway.go": "discord",
		"internal/plugins/web/web.go":         "plugin.*",
		"internal/memory/skills/x.go":         "skills",
		"internal/memory/semantic/x.go":       "memory",
		"reference/logging/tenantlog.go":      "",
	} {
		if got := providerOf(file); got != want {
			t.Errorf("providerOf(%s) = %q, want %q", file, got, want)
		}
	}
}
