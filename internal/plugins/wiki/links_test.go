package wiki

// White-box tests for the Obsidian parsing/resolution internals.

import (
	"reflect"
	"testing"
)

func TestSplitFrontmatter(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantAliases []string
		wantTags    []string
		wantBody    string
	}{
		{"none", "# Title\nbody", nil, nil, "# Title\nbody"},
		{"inline-list", "---\naliases: [Foo, Bar]\ntags: [a, b/c]\n---\nbody here", []string{"Foo", "Bar"}, []string{"a", "b/c"}, "body here"},
		{"block-list", "---\naliases:\n  - Foo\n  - Bar\ntags:\n  - x\n---\nB", []string{"Foo", "Bar"}, []string{"x"}, "B"},
		{"scalar-csv", "---\naliases: Foo, Bar\ntags: alpha beta\n---\nB", []string{"Foo", "Bar"}, []string{"alpha", "beta"}, "B"},
		{"singular-keys", "---\nalias: Solo\ntag: one\n---\nB", []string{"Solo"}, []string{"one"}, "B"},
		{"malformed-yaml-still-strips", "---\n: : bad\n---\nclean body", nil, nil, "clean body"},
	}
	for _, c := range cases {
		a, tg, body := splitFrontmatter(c.in)
		if !eq(a, c.wantAliases) || !eq(tg, c.wantTags) || body != c.wantBody {
			t.Errorf("%s: got aliases=%v tags=%v body=%q; want %v / %v / %q",
				c.name, a, tg, body, c.wantAliases, c.wantTags, c.wantBody)
		}
	}
}

func TestExtractWikiLinks(t *testing.T) {
	body := "See [[Goroutines]] and [[Channels|chans]] plus [[Errors#Wrapping]].\n" +
		"Embed ![[Diagram]]. Folder [[notes/Deep]]. Skip [[#LocalOnly]] and [[]].\n" +
		"```\n[[InCodeIgnored]]\n```\ninline `[[AlsoIgnored]]` end."
	got := extractWikiLinks(stripCode(body))
	want := []linkTarget{
		{Target: "Goroutines"},
		{Target: "Channels", Alias: "chans"},
		{Target: "Errors", Heading: "Wrapping"},
		{Target: "Diagram", Embed: true},
		{Target: "notes/Deep"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links\n got=%+v\nwant=%+v", got, want)
	}
}

func TestExtractTags(t *testing.T) {
	body := "#go #nested/topic intro. Not a # heading. email a#b not tag.\n" +
		"#123 is not a tag (numeric). Dup #go again.\n```\n#incode\n```"
	got := extractTags(stripCode(body))
	want := []string{"go", "nested/topic"}
	if !eq(got, want) {
		t.Fatalf("tags got=%v want=%v", got, want)
	}
}

func TestResolver(t *testing.T) {
	notes := map[string]noteMeta{
		"Goroutines.md":      {Aliases: []string{"Green Threads"}},
		"topics/Channels.md": {},
		"a/Dup.md":           {},
		"b/Dup.md":           {},
	}
	r := newResolver(notes)
	check := func(target, want string, wantOK bool) {
		got, ok := r.resolve(target)
		if got != want || ok != wantOK {
			t.Errorf("resolve(%q)=%q,%v want %q,%v", target, got, ok, want, wantOK)
		}
	}
	check("Goroutines", "Goroutines.md", true)           // basename
	check("goroutines", "Goroutines.md", true)           // case-insensitive
	check("Green Threads", "Goroutines.md", true)        // alias
	check("Channels", "topics/Channels.md", true)        // basename in subdir
	check("topics/Channels", "topics/Channels.md", true) // path-qualified
	check("Dup", "a/Dup.md", true)                       // ambiguous → smallest path
	check("Nope", "", false)                             // unresolved
}

func eq(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
