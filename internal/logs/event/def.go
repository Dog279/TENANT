package event

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Provider is a named source of events, writing into one log. Name and Log
// are fixed when it registers.
type Provider struct {
	Name, Log string
	defs      map[uint32]*Def
}

// Def is one catalogued event: an ID within its provider, a level, a task, a
// message template and the fields it takes. Build it at package init with
// Provider.Event and the methods that return it; it must not change once
// events are written.
type Def struct {
	spec  Spec
	holes [][]int // the template's {field} placeholders, as holeRe submatch indexes
}

// Spec is a definition as the catalog documents it.
type Spec struct {
	Provider, Log          string
	ID                     uint32
	Level                  Level
	Channel                Channel
	Task, Template         string
	Fields, Sensitive      []string
	Keywords               []string
	Meaning, Cause, Action string
	Retired                bool
}

var (
	regMu     sync.Mutex
	providers = map[string]*Provider{}

	// nameRe is the shape of a provider's or a log's name (ValidName).
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)
	fieldRe = regexp.MustCompile(`^[A-Za-z0-9_.]+$`)
	holeRe  = regexp.MustCompile(`\{([A-Za-z0-9_.]+)\}`)
)

// ValidName reports whether s can name a provider or a log. A log name is
// also a folder name on Windows, Linux and macOS, so beyond nameRe's shape a
// Windows device name (con, prn, aux, nul, com0–com9, lpt0–lpt9, with or
// without an extension) and a name ending in a dot are refused.
func ValidName(s string) bool {
	if !nameRe.MatchString(s) || strings.HasSuffix(s, ".") {
		return false
	}
	base, _, _ := strings.Cut(s, ".")
	switch {
	case base == "con", base == "prn", base == "aux", base == "nul":
		return false
	case len(base) == 4 && (base[:3] == "com" || base[:3] == "lpt") && base[3] >= '0' && base[3] <= '9':
		return false
	}
	return true
}

// NewProvider registers a provider writing into log. A bad name, or a name
// registered twice, panics: providers are declared at package init.
func NewProvider(name, log string) *Provider {
	if !ValidName(name) || !ValidName(log) {
		panic(fmt.Sprintf("event: bad provider name %q or log name %q", name, log))
	}
	regMu.Lock()
	defer regMu.Unlock()
	if providers[name] != nil {
		panic(fmt.Sprintf("event: provider %q registered twice", name))
	}
	p := &Provider{Name: name, Log: log, defs: map[uint32]*Def{}}
	providers[name] = p
	return p
}

// Providers lists every registered provider, by name.
func Providers() []*Provider {
	regMu.Lock()
	defer regMu.Unlock()
	return slices.SortedFunc(maps.Values(providers), func(a, b *Provider) int { return strings.Compare(a.Name, b.Name) })
}

// Lookup finds definition id of provider.
func Lookup(provider string, id uint32) (*Def, bool) {
	regMu.Lock()
	defer regMu.Unlock()
	if p := providers[provider]; p != nil {
		d, ok := p.defs[id]
		return d, ok
	}
	return nil, false
}

// Defs lists p's definitions, by ID.
func (p *Provider) Defs() []*Def {
	regMu.Lock()
	defer regMu.Unlock()
	return slices.SortedFunc(maps.Values(p.defs), func(a, b *Def) int { return cmp.Compare(a.spec.ID, b.spec.ID) })
}

// Event registers event id of p. tmpl is its message, naming fields as
// {field}. An ID of 0 or one p already has, a bad level, or a placeholder
// that is not one of fields panics.
func (p *Provider) Event(id uint32, lvl Level, task, tmpl string, fields ...string) *Def {
	d := &Def{
		spec:  Spec{Provider: p.Name, Log: p.Log, ID: id, Level: lvl, Task: task, Template: tmpl, Fields: fields},
		holes: holeRe.FindAllStringSubmatchIndex(tmpl, -1),
	}
	must(d.check(&d.spec))
	regMu.Lock()
	defer regMu.Unlock()
	if p.defs[id] != nil {
		panic(fmt.Sprintf("event: %s: ID registered twice", d.name()))
	}
	p.defs[id] = d
	return d
}

// Sensitive marks fields as sensitive: personal content or a payload, kept
// but masked. Only declared fields the template doesn't name can be.
func (d *Def) Sensitive(fields ...string) *Def {
	s := d.spec
	s.Sensitive = append(slices.Clip(s.Sensitive), fields...)
	must(d.check(&s))
	d.spec = s
	return d
}

// Doc documents the event: what it means, why it happens, and what to do.
func (d *Def) Doc(meaning, cause, action string) *Def {
	if meaning == "" || cause == "" || action == "" {
		panic(fmt.Sprintf("event: %s: Doc needs a meaning, a cause and an action", d.name()))
	}
	d.spec.Meaning, d.spec.Cause, d.spec.Action = meaning, cause, action
	return d
}

// Keywords tags the event, for example audit.failure or config.
func (d *Def) Keywords(kw ...string) *Def {
	d.spec.Keywords = slices.Clip(append(d.spec.Keywords, kw...))
	return d
}

// Diagnostic moves the event to the diagnostic channel.
func (d *Def) Diagnostic() *Def {
	d.spec.Channel = Diagnostic
	return d
}

// Retired marks an ID that is no longer written. It stays in the catalog,
// so the ID is never reused.
func (d *Def) Retired() *Def {
	d.spec.Retired = true
	return d
}

// Spec returns a copy of the definition.
func (d *Def) Spec() Spec {
	s := d.spec
	s.Fields, s.Sensitive, s.Keywords = slices.Clone(s.Fields), slices.Clone(s.Sensitive), slices.Clone(s.Keywords)
	return s
}

// Validate checks every registered definition again. Registration panics on
// a bad one already; a test calls Validate so the whole catalog is checked
// in CI too.
func Validate() error {
	var errs []error
	for _, p := range Providers() {
		for _, d := range p.Defs() {
			errs = append(errs, d.check(&d.spec))
		}
	}
	return errors.Join(errs...)
}

// check holds a definition to the catalog's rules: a real ID and level,
// fields declared once, and a template that names only declared,
// non-sensitive fields, so Msg never carries a sensitive value.
func (d *Def) check(s *Spec) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("event: %s: %s", d.name(), fmt.Sprintf(format, args...))
	}
	if s.ID == 0 {
		return fail("ID 0 is for uncatalogued events")
	}
	if s.Level < Critical || s.Level > Verbose {
		return fail("bad level %d", s.Level)
	}
	for i, f := range s.Fields {
		if !fieldRe.MatchString(f) || slices.Contains(s.Fields[:i], f) {
			return fail("field %q is malformed or declared twice", f)
		}
	}
	for _, f := range s.Sensitive {
		if !slices.Contains(s.Fields, f) {
			return fail("sensitive field %q is not declared", f)
		}
	}
	for _, h := range d.holes {
		switch f := s.Template[h[2]:h[3]]; {
		case !slices.Contains(s.Fields, f):
			return fail("the template names {%s}, which is not a declared field", f)
		case slices.Contains(s.Sensitive, f):
			return fail("the template names {%s}, which is sensitive", f)
		}
	}
	return nil
}

func (d *Def) name() string { return fmt.Sprintf("%s/%d", d.spec.Provider, d.spec.ID) }

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// Render is d's message for e: its template filled from e's data. A view
// that shows an event differently from how it was written renders it again
// (agent.RowMsg).
func (d *Def) Render(e *Event) string { return d.render(e) }

// render fills the template's placeholders from e's data, or its actor and
// surface (attribution.go). One with no value stays as written, so a missing
// field shows.
func (d *Def) render(e *Event) string {
	t := d.spec.Template
	if len(d.holes) == 0 {
		return t
	}
	var b strings.Builder
	last := 0
	for _, h := range d.holes {
		b.WriteString(t[last:h[0]])
		last = h[1]
		key := t[h[2]:h[3]]
		switch i := slices.IndexFunc(e.Data, func(f Field) bool { return f.Key == key }); {
		case i >= 0:
			b.WriteString(e.Data[i].Text())
		case key == "actor" && e.Actor != "":
			b.WriteString(e.Actor)
		case key == "surface" && e.Surface != "":
			b.WriteString(e.Surface)
		default:
			b.WriteString(t[h[0]:h[1]])
		}
	}
	b.WriteString(t[last:])
	return b.String()
}
