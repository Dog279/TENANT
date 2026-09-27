package logs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// minSecretLen is the shortest known value the scrubber looks for; shorter ones
// would match ordinary words. Shorter text is returned as it is: the shortest
// shape (token=ab) is this long too.
const minSecretLen = 8

// scrubWindow is how far past the value cap a long value is scrubbed. The rest
// is dropped by the cap anyway, so a huge tool result costs a bounded scan, and
// a secret up to half the window long that straddles the cut is still seen
// whole.
const scrubWindow = 8 << 10

// Scrubber replaces secrets in log text before anything keeps it (logging v2
// §11): the values Tenant knows are secret (SetSecrets), raw and in the
// encodings they get logged in, become «secret:<name>»; text shaped like a
// credential (a bearer token, a JWT, an API key, a PEM private key, a
// password in a URL, password=… and "token": "…") keeps its label and loses
// the secret part. The zero value knows no values and scrubs the shapes. Safe
// for concurrent use, SetSecrets included.
type Scrubber struct {
	known atomic.Pointer[knownSecrets]
}

type knownSecrets struct {
	named map[string]string // as given, so an unchanged set skips the rebuild
	r     *strings.Replacer // nil when no value is long enough
	min   int               // the shortest needle: shorter text skips r
}

// SetSecrets replaces the known values, name → value. Values shorter than
// minSecretLen are ignored. Each is matched raw, JSON-escaped, URL-encoded and
// base64-encoded (standard and URL alphabets, padded or not, and inside a
// longer encoded value), and replaced with «secret:<name>».
func (s *Scrubber) SetSecrets(named map[string]string) {
	if cur := s.known.Load(); cur != nil && maps.Equal(cur.named, named) {
		return
	}
	type needle struct{ text, repl string }
	var needles []needle
	seen := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(named)) { // sorted: a value two names share keeps the first
		v := named[name]
		if len(v) < minSecretLen {
			continue
		}
		for _, f := range secretForms(v) {
			if !seen[f] {
				seen[f] = true
				needles = append(needles, needle{f, "«secret:" + name + "»"})
			}
		}
	}
	// Longest first: a Replacer prefers the earlier of two matches at one
	// position, so a value that contains another is replaced whole.
	slices.SortStableFunc(needles, func(a, b needle) int { return len(b.text) - len(a.text) })
	k := &knownSecrets{named: maps.Clone(named)}
	if len(needles) > 0 {
		pairs := make([]string, 0, 2*len(needles))
		for _, n := range needles {
			pairs = append(pairs, n.text, n.repl)
		}
		k.r, k.min = strings.NewReplacer(pairs...), len(needles[len(needles)-1].text)
	}
	s.known.Store(k)
}

// secretForms is v the ways it shows up in a log line.
func secretForms(v string) []string {
	forms := []string{v, url.QueryEscape(v), url.PathEscape(v)}
	if b, err := json.Marshal(v); err == nil { // escapes <, > and & as Go does
		forms = append(forms, string(b[1:len(b)-1]))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // as most other encoders do
	if enc.Encode(v) == nil {
		b := bytes.TrimSpace(buf.Bytes())
		forms = append(forms, string(b[1:len(b)-1]))
	}
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		forms = append(forms, base64Forms(v, e)...)
	}
	return forms
}

// base64Forms is v in one base64 alphabet: whole, padded and not, and the
// characters that encode v alone when it sits inside a longer encoded value
// (a Basic credential, say) at each of the three byte offsets it can start at.
func base64Forms(v string, enc *base64.Encoding) []string {
	raw := enc.WithPadding(base64.NoPadding)
	forms := []string{enc.EncodeToString([]byte(v)), raw.EncodeToString([]byte(v))}
	for k := range 3 {
		s := raw.EncodeToString(append(make([]byte, k), v...))
		head := [3]int{0, 2, 3}[k] // characters that also encode the k bytes before v
		tail := 0
		if (k+len(v))%3 != 0 {
			tail = 1 // the last character also encodes what follows v
		}
		forms = append(forms, s[head:len(s)-tail])
	}
	return forms
}

// detector finds one shape of credential. Its expression runs only on text
// at least min bytes long that passes hint, a cheap test (lower is the text
// lower-cased), so a line with nothing secret-shaped costs a few substring
// checks.
type detector struct {
	min  int
	hint func(text, lower string) bool
	re   *regexp.Regexp
	repl string
}

// secretKey is the tail of a key that names a credential: password=, "token":,
// client_secret=, x-api-key=… (not tokens=, token_type or max_tokens).
const secretKey = `(?:passw(?:or)?d|secret|token|api[_-]?key)`

// keyHint passes text that may hold a secretKey; jsonHint, text that may hold
// one as a JSON key.
var keyHint = inLower("passw", "token", "secret", "key")

func jsonHint(t, l string) bool {
	return strings.IndexByte(t, '"') >= 0 && strings.IndexByte(t, ':') >= 0 && keyHint(t, l)
}

// detectors run in order: specific shapes first, then the key=value and JSON
// forms. None matches a «secret:…» marker, so scrubbing twice changes nothing.
// A prefix that also occurs inside ordinary words (disk-, desk_test_) needs a
// word boundary; the distinctive ones match anywhere, so a token glued to
// other text (%20ghp_…) is still found.
var detectors = []detector{
	// A PEM private key, header to footer, or to the end of a value cut short.
	{27, in("PRIVATE KEY"), regexp.MustCompile(`(-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----)[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z)`), "${1}«secret:private-key»${2}"},
	// A JWT: base64url JSON header and payload, then the signature.
	{16, in("eyJ"), regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`), "«secret:jwt»"},
	{23, inLower("bearer"), regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/-]{16,}=*`), "${1}«secret:bearer»"},
	{23, in("sk-"), regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), "«secret:api-key»"},
	{18, in("_live_", "_test_"), regexp.MustCompile(`\b[rs]k_(?:live|test)_[A-Za-z0-9]{10,}`), "«secret:stripe»"},
	{34, in("ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"), regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})`), "«secret:github»"},
	{15, in("xox"), regexp.MustCompile(`xox[a-z]-[A-Za-z0-9-]{10,}`), "«secret:slack»"},
	{39, in("AIza"), regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), "«secret:google»"},
	{22, in("tskey-"), regexp.MustCompile(`tskey-[A-Za-z0-9-]{16,}`), "«secret:tailscale»"},
	{20, in("AKIA", "ASIA"), regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "«secret:aws»"},
	{56, inLower("accesskey", "access_key"), regexp.MustCompile(`(?i)(secret_?access_?key["']?\s*[=:]\s*["']?)[A-Za-z0-9/+]{40,}`), "${1}«secret:aws»"},
	{28, in("/api/webhooks/"), regexp.MustCompile(`(discord(?:app)?\.com/api/webhooks/\d+/)[A-Za-z0-9_-]+`), "${1}«secret:discord-webhook»"},
	{59, discordShape, regexp.MustCompile(`\b[MNO][A-Za-z0-9_-]{23,27}\.[A-Za-z0-9_-]{6}\.[A-Za-z0-9_-]{27,}`), "«secret:discord»"},
	// user:password@ in a URL: the password goes, the user stays.
	{8, func(t, _ string) bool { return strings.Contains(t, "://") && strings.IndexByte(t, '@') >= 0 },
		regexp.MustCompile(`(\b[A-Za-z][A-Za-z0-9+.-]*://[^\s/?#@:]*:)[^\s/?#@«][^\s/?#]*(@)`), "${1}«secret:url-password»${2}"},
	// password=…, access_token=…: a query string, a command line, a config dump.
	{8, func(t, l string) bool { return strings.IndexByte(t, '=') >= 0 && keyHint(t, l) },
		regexp.MustCompile(`(?i)([a-z0-9_.-]*` + secretKey + `=)[^\s&"'«][^\s&"']*`), "${1}«secret:param»"},
	// "password": "…" in JSON, and \"password\": \"…\" in JSON held in a string.
	{10, jsonHint, regexp.MustCompile(`(?i)("[a-z0-9_-]*` + secretKey + `"\s*:\s*")(?:[^"\\«]|\\.)(?:[^"\\]|\\.)*`), "${1}«secret:json»"},
	{13, func(t, l string) bool { return strings.Contains(t, `\"`) && keyHint(t, l) },
		regexp.MustCompile(`(?i)(\\"[a-z0-9_-]*` + secretKey + `\\"\s*:\s*\\")(?:[^"\\«]|\\[^"])(?:[^"\\]|\\[^"])*`), "${1}«secret:json»"},
}

// in and inLower are hints: the text, or the lower-cased text, contains one
// of words.
func in(words ...string) func(string, string) bool {
	return func(t, _ string) bool { return containsAny(t, words) }
}

func inLower(words ...string) func(string, string) bool {
	return func(_, l string) bool { return containsAny(l, words) }
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// secretField reports whether a field key names a credential (secretKey's
// words, at its end): its value is replaced whole, whatever it looks like.
func secretField(key string) bool {
	k := strings.ToLower(key)
	for _, w := range []string{"password", "passwd", "secret", "token", "apikey", "api_key", "api-key"} {
		if strings.HasSuffix(k, w) {
			return true
		}
	}
	return false
}

// discordShape is the Discord bot token hint: three dot-separated parts, the
// middle one six characters long.
func discordShape(t, _ string) bool {
	for {
		i := strings.IndexByte(t, '.')
		if i < 0 || i+7 >= len(t) {
			return false
		}
		if t[i+7] == '.' {
			return true
		}
		t = t[i+1:]
	}
}

// Scrub returns text with every secret it holds replaced.
func (s *Scrubber) Scrub(text string) string {
	if len(text) < minSecretLen {
		return text
	}
	if k := s.known.Load(); k != nil && k.r != nil && len(text) >= k.min {
		text = k.r.Replace(text)
	}
	lower := strings.ToLower(text)
	for _, d := range detectors {
		if len(text) >= d.min && d.hint(text, lower) {
			text = d.re.ReplaceAllString(text, d.repl)
		}
	}
	return text
}

// ScrubCap scrubs v, then cuts it to max bytes (max <= 0: no cut). The order
// matters: cut first, and a secret straddling the cut would leave its first
// part behind. A value far longer than max is scrubbed only scrubWindow past
// the cut.
func (s *Scrubber) ScrubCap(v string, max int) string {
	if max > 0 && len(v) > max+scrubWindow {
		// Replacements can shorten the window. If they pulled its unscrubbed
		// end under the cut, scrub the whole value instead.
		if head := s.Scrub(v[:max+scrubWindow]); len(head) >= max+scrubWindow/2 {
			cut := max
			for cut > 0 && !utf8.RuneStart(head[cut]) {
				cut--
			}
			return head[:cut] + "…(+" + strconv.Itoa(len(head)-cut+len(v)-max-scrubWindow) + " bytes)"
		}
	}
	return capValue(s.Scrub(v), max)
}

// Entry scrubs e's message and every field value, then caps each at max bytes
// (max <= 0: no cap). A field whose key names a credential (password, token,
// secret, api_key) loses its whole value. The caller's Fields are not changed.
func (s *Scrubber) Entry(e Entry, max int) Entry {
	e.Msg = s.ScrubCap(e.Msg, max)
	copied := false
	for i, f := range e.Fields {
		v := s.ScrubCap(f.Value, max)
		if v != "" && secretField(f.Key) && !strings.HasPrefix(v, "«secret:") {
			v = "«secret:" + f.Key + "»"
		}
		if v != f.Value {
			if !copied {
				e.Fields, copied = slices.Clone(e.Fields), true
			}
			e.Fields[i].Value = v
		}
	}
	return e
}

// SetSecrets hands the hub the values its scrubber replaces wherever they
// appear (see Scrubber.SetSecrets). The composition root collects them from
// the secrets layer and calls this again whenever one changes.
func (h *Hub) SetSecrets(named map[string]string) { h.scrub.SetSecrets(named) }

// shapesOnly scrubs for Scrub before a hub is installed.
var shapesOnly Scrubber

// Scrub applies the installed hub's scrubber to text that is cut or printed
// before it reaches the log book: a command clipped for its log line, a
// daemon's stdout and stderr. Before Install it scrubs the shapes only.
func Scrub(text string) string {
	if h := Default(); h != nil {
		return h.scrub.Scrub(text)
	}
	return shapesOnly.Scrub(text)
}

// ScrubWriter scrubs everything written through it, for the copy of the log a
// supervisor captures from a daemon's stderr. Each write must hold whole
// lines, as slog's handlers write them.
func ScrubWriter(w io.Writer) io.Writer { return scrubWriter{w} }

type scrubWriter struct{ w io.Writer }

func (s scrubWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(s.w, Scrub(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
