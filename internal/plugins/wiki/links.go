package wiki

// Obsidian-link awareness. Kept in one readable file: frontmatter
// split, [[wikilink]] / #tag extraction, and basename resolution —
// no graph library, just maps and slices (same Karpathy discipline as
// the rest of this package). The link GRAPH (forward/back) is derived
// in wiki.go from the raw targets stored here.

import (
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// noteMeta is the per-note graph data persisted in the sidecar. Raw
// targets (what the author actually typed) are stored, not resolved
// paths: resolution depends on the whole vault, so a renamed/added
// note must be able to re-link everything without re-embedding.
type noteMeta struct {
	Aliases []string      `json:"aliases,omitempty"`
	Tags    []string      `json:"tags,omitempty"`
	Targets []linkTarget  `json:"targets,omitempty"`
	Virtual []VirtualLink `json:"virtual,omitempty"` // auto-derived semantic edges (sidecar-only)
}

// VirtualLink is an embedding-derived (semantic) relationship between two notes,
// discovered at reindex from chunk cosine similarity and stored ONLY in the
// sidecar — never written back to the markdown (canonical files stay read-only,
// the Karpathy ethos). It gives graph expansion edges to traverse on notes the
// author never manually linked.
type VirtualLink struct {
	Target string  `json:"target"` // resolved note path (same key space as manual targets)
	Score  float64 `json:"score"`  // cosine of the best chunk pair, [0,1]
}

// linkTarget is one [[wikilink]] as written.
type linkTarget struct {
	Target  string `json:"target"`            // note name/path as typed ("" ⇒ same-note heading link)
	Heading string `json:"heading,omitempty"` // #Heading or #^block
	Alias   string `json:"alias,omitempty"`   // |display text
	Embed   bool   `json:"embed,omitempty"`   // ![[...]] transclusion
}

// display is what a human/author calls this link (best signal text).
func (l linkTarget) display() string {
	switch {
	case l.Alias != "":
		return l.Alias
	case l.Target != "":
		return l.Target
	default:
		return l.Heading
	}
}

var (
	fmRE     = regexp.MustCompile(`(?s)\A---[ \t]*\r?\n(.*?)\r?\n(?:---|\.\.\.)[ \t]*(?:\r?\n|\z)(.*)`)
	wikiRE   = regexp.MustCompile(`(!?)\[\[\s*([^\[\]]+?)\s*\]\]`)
	fenceRE  = regexp.MustCompile("(?s)```.*?```")
	inlineRE = regexp.MustCompile("`[^`]*`")
	// A tag: # at a boundary, then a non-space run; not "# heading"
	// (space after #), not "word#frag" (preceded by a word char).
	tagRE = regexp.MustCompile(`(?:^|[\s(>\[!])#([A-Za-z0-9_][A-Za-z0-9_/-]*)`)
)

// flexList accepts the three shapes Obsidian writes aliases/tags in:
// a YAML list, or a single scalar that may be comma/space separated.
type flexList []string

func (f *flexList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		var xs []string
		if err := n.Decode(&xs); err != nil {
			return err
		}
		*f = normList(xs)
	case yaml.ScalarNode:
		*f = normList(splitLoose(n.Value))
	}
	return nil
}

type fmDoc struct {
	Aliases flexList `yaml:"aliases"`
	Alias   flexList `yaml:"alias"`
	Tags    flexList `yaml:"tags"`
	Tag     flexList `yaml:"tag"`
}

// splitFrontmatter peels a leading `---` YAML block off the note. The
// block is ALWAYS removed from the body (so it isn't chunked/embedded
// as prose) even if the YAML is malformed — matching Obsidian.
func splitFrontmatter(content string) (aliases, tags []string, body string) {
	m := fmRE.FindStringSubmatch(content)
	if m == nil {
		return nil, nil, content
	}
	var d fmDoc
	_ = yaml.Unmarshal([]byte(m[1]), &d) // best-effort; body still stripped
	aliases = normList(append(append([]string{}, d.Aliases...), d.Alias...))
	tags = normTags(append(append([]string{}, d.Tags...), d.Tag...))
	return aliases, tags, m[2]
}

// parseNote returns the graph metadata + the prose body to chunk.
func parseNote(content string) (noteMeta, string) {
	aliases, fmTags, body := splitFrontmatter(content)
	clean := stripCode(body) // links/tags inside code are not real links
	meta := noteMeta{
		Aliases: aliases,
		Tags:    mergeUnique(fmTags, extractTags(clean)),
		Targets: extractWikiLinks(clean),
	}
	return meta, body
}

func stripCode(s string) string {
	s = fenceRE.ReplaceAllString(s, " ")
	return inlineRE.ReplaceAllString(s, " ")
}

func extractWikiLinks(body string) []linkTarget {
	var out []linkTarget
	for _, m := range wikiRE.FindAllStringSubmatch(body, -1) {
		inner := strings.TrimSpace(m[2])
		if inner == "" {
			continue
		}
		var lt linkTarget
		lt.Embed = m[1] == "!"
		if i := strings.Index(inner, "|"); i >= 0 {
			lt.Alias = strings.TrimSpace(inner[i+1:])
			inner = strings.TrimSpace(inner[:i])
		}
		if i := strings.Index(inner, "#"); i >= 0 {
			lt.Heading = strings.TrimSpace(inner[i+1:])
			inner = strings.TrimSpace(inner[:i])
		}
		lt.Target = inner
		if lt.Target == "" {
			continue // [[#Heading]] — same-note anchor, no cross-note edge
		}
		out = append(out, lt)
	}
	return out
}

func extractTags(body string) []string {
	var out []string
	for _, m := range tagRE.FindAllStringSubmatch(body, -1) {
		t := strings.ToLower(strings.Trim(m[1], "/"))
		if t == "" || isAllDigitsOrSlash(t) { // Obsidian: needs a non-numeric char
			continue
		}
		out = append(out, t)
	}
	return uniqueStable(out)
}

// --- resolution (Obsidian default: by basename, case-insensitive,
// then alias; ambiguous ⇒ lexicographically-smallest path) ---

type resolver struct {
	byKey map[string]string // lowercased basename|relpath|alias → rel path
}

func newResolver(notes map[string]noteMeta) *resolver {
	r := &resolver{byKey: map[string]string{}}
	put := func(k, rel string) {
		k = strings.ToLower(k)
		if k == "" {
			return
		}
		if cur, ok := r.byKey[k]; !ok || rel < cur {
			r.byKey[k] = rel
		}
	}
	for rel, m := range notes {
		slash := filepath.ToSlash(rel)
		put(slash, rel)
		put(strings.TrimSuffix(slash, filepath.Ext(slash)), rel)
		put(baseNoExt(slash), rel)
		for _, a := range m.Aliases {
			put(a, rel)
		}
	}
	return r
}

// resolve maps a typed target to a note path. ok=false ⇒ unresolved
// (a link to a note that doesn't exist — still kept as weak signal).
func (r *resolver) resolve(target string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(filepath.ToSlash(target)))
	if t == "" {
		return "", false
	}
	for _, k := range []string{t, strings.TrimSuffix(t, ".md"), strings.TrimSuffix(t, ".markdown"), baseNoExt(t)} {
		if rel, ok := r.byKey[k]; ok {
			return rel, true
		}
	}
	return "", false
}

// --- small helpers ---

func baseNoExt(p string) string {
	b := path_Base(p)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// path_Base avoids importing path just for one call; slugs are slash-form.
func path_Base(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func splitLoose(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

func normList(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return uniqueStable(out)
}

func normTags(xs []string) []string {
	var out []string
	for _, x := range xs {
		x = strings.ToLower(strings.Trim(strings.TrimSpace(x), "#/"))
		if x != "" && !isAllDigitsOrSlash(x) {
			out = append(out, x)
		}
	}
	return uniqueStable(out)
}

func isAllDigitsOrSlash(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r == '/') {
			return false
		}
	}
	return true
}

func uniqueStable(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func mergeUnique(a, b []string) []string { return uniqueStable(append(append([]string{}, a...), b...)) }
