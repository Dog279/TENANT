package logs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fake assembles a credential-shaped fixture at run time, so no line of this
// file looks like a live credential to a secret scanner (gitleaks runs in CI).
func fake(parts ...string) string { return strings.Join(parts, "") }

// Every shape §11 lists, and near misses that must come through untouched.
func TestScrub_Corpus(t *testing.T) {
	body := "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789" // 36 characters
	jwt := fake("eyJhbGciOiJIUzI1NiJ9", ".", "eyJzdWIiOiIxMjM0NTY3ODkwIn0", ".", body)
	begin, end := fake("-----BEGIN RSA ", "PRIVATE KEY-----"), fake("-----END RSA ", "PRIVATE KEY-----")
	cases := []struct{ name, in, want string }{
		{"bearer header", fake("Authorization: Bearer ", body), "Authorization: Bearer «secret:bearer»"},
		{"bearer lower case", fake(`curl -H "authorization: bearer `, body, `=" x`), `curl -H "authorization: bearer «secret:bearer»" x`},
		{"jwt", "id_token " + jwt + " ok", "id_token «secret:jwt» ok"},
		{"bearer jwt", "Bearer " + jwt, "Bearer «secret:jwt»"},
		{"url-encoded jwt", "?q=a%20" + jwt, "?q=a%20«secret:jwt»"},
		{"openai key", fake("key sk-", "proj-", body), "key «secret:api-key»"},
		{"anthropic key", fake("sk-", "ant-api03-", body, "-yz"), "«secret:api-key»"},
		{"stripe live", fake("sk_", "live_", body, " and rk_", "live_", body), "«secret:stripe» and «secret:stripe»"},
		{"github classic", fake("gh", "p_", body), "«secret:github»"},
		{"github fine-grained", fake("github_", "pat_11ABCDEFG0123456789_", body), "«secret:github»"},
		{"github url-encoded", fake("q=a%20gh", "o_", body), "q=a%20«secret:github»"},
		{"slack", fake("xo", "xb-123456789012-", body), "«secret:slack»"},
		{"google", fake("AI", "za", body[:35]), "«secret:google»"},
		{"tailscale", fake("tsk", "ey-auth-", body), "«secret:tailscale»"},
		{"aws key id", fake("AK", "IA", "Q2WSXCDE3RFVBGT5"), "«secret:aws»"},
		{"aws secret", fake("aws_secret_access_key = ", body, "/+ab"), "aws_secret_access_key = «secret:aws»"},
		{"discord bot token", fake("bot MTA4NjE2NTQ0MzQ1MjMyNzk4Mg", ".GAbCdE.", body, "_-"), "bot «secret:discord»"},
		{"discord webhook", fake("https://discord.com/api/", "webhooks/123456789012345678/", body), "https://discord.com/api/webhooks/123456789012345678/«secret:discord-webhook»"},
		{"pem private key", "key=" + begin + "\n" + body + "\n" + body + "\n" + end + " end", "key=" + begin + "«secret:private-key»" + end + " end"},
		{"pem cut short", fake("-----BEGIN OPENSSH ", "PRIVATE KEY-----\n", body), fake("-----BEGIN OPENSSH ", "PRIVATE KEY-----«secret:private-key»")},
		{"url password", fake("postgres://admin:", "hunter22@db.local:5432/app"), "postgres://admin:«secret:url-password»@db.local:5432/app"},
		{"url password with @", fake("https://bob:", "p@ss@host/x"), "https://bob:«secret:url-password»@host/x"},
		{"query token", "GET /cb?code=1&token=s3cr3tValue&x=1", "GET /cb?code=1&token=«secret:param»&x=1"},
		{"flag password", "mysql --password=hunter22 db", "mysql --password=«secret:param» db"},
		{"env var", "GITHUB_TOKEN=abc123 DB_PASSWORD=xyz ./run", "GITHUB_TOKEN=«secret:param» DB_PASSWORD=«secret:param» ./run"},
		{"client secret", "client_secret=abcdef&api_key=12345", "client_secret=«secret:param»&api_key=«secret:param»"},
		{"json fields", `{"user":"bob","password":"hunter2","api_key": "k\"1"}`, `{"user":"bob","password":"«secret:json»","api_key": "«secret:json»"}`},
		{"json token", `{"access_token":"ya29.abc","token_type":"Bearer"}`, `{"access_token":"«secret:json»","token_type":"Bearer"}`},
		{"json in a string", `{"text":"{\"secret\":\"shh-it-is\",\"n\":1}"}`, `{"text":"{\"secret\":\"«secret:json»\",\"n\":1}"}`},

		// Near misses: ordinary text stays as it is.
		{"plain", "tool call web_search", ""},
		{"token counts", "team budget tokens=1234/50000 max_tokens=4096 est_tokens=12", ""},
		{"token type", `{"token_type":"Bearer","expires_in":3600,"max_tokens":100}`, ""},
		{"prose bearer", "the bearer of bad news", ""},
		{"short sk", "task-runner sk-learn", ""},
		{"sk inside a word", fake("disk-usage-report-generator-v2 desk_", "test_", body), ""},
		{"url without password", "dashboard: serving on http://127.0.0.1:9770/api/activity", ""},
		{"scp url", "git clone git@github.com:org/repo.git", ""},
		{"email", "mail bob@example.com about https://example.com/x", ""},
		{"dotted", "version 1.2.3 in file.name.go, e.g. this", ""},
		{"timezone", "cron: timezone Asia/Tokyo", ""},
		{"certificate", "-----BEGIN CERTIFICATE-----\nMIIB", ""},
		{"password words", "password reset sent; passwords=3; secret: rotated", ""},
		{"already scrubbed", `token=«secret:x» "password":"«secret:y»" https://u:«secret:z»@h`, ""},
	}
	var s Scrubber
	for _, c := range cases {
		want := c.want
		if want == "" {
			want = c.in
		}
		if got := s.Scrub(c.in); got != want {
			t.Errorf("%s:\n in   %q\n got  %q\n want %q", c.name, c.in, got, want)
		}
		if got := s.Scrub(s.Scrub(c.in)); got != want {
			t.Errorf("%s: scrubbing twice changed the result: %q", c.name, got)
		}
	}
}

// A known value is found raw, JSON-escaped, URL-encoded and base64-encoded,
// also inside a longer encoded value at every byte offset.
func TestScrub_KnownValueForms(t *testing.T) {
	value := fake(`Tr0ub4dor&3 `, `"horse"/staple`)
	var s Scrubber
	s.SetSecrets(map[string]string{"example": value, "short": "1234567"})
	jsonText, _ := json.Marshal(value)
	var forms []string
	forms = append(forms, value, url.QueryEscape(value), url.PathEscape(value), string(jsonText))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		forms = append(forms, enc.EncodeToString([]byte(value)))
		for _, prefix := range []string{"", "u", "us", "user:"} { // Basic credentials: user:password
			forms = append(forms, enc.EncodeToString([]byte(prefix+value+"!tail")))
		}
	}
	for _, f := range forms {
		got := s.Scrub("value " + f + " end")
		if !strings.Contains(got, "«secret:example»") {
			t.Errorf("form %q not scrubbed: %q", f, got)
		}
	}
	if got := s.Scrub("pin 1234567 ok"); got != "pin 1234567 ok" {
		t.Errorf("a value under 8 bytes must be ignored: %q", got)
	}

	s.SetSecrets(map[string]string{"a": "abcdefgh", "b": "abcdefgh-ijkl"})
	if got := s.Scrub("x abcdefgh-ijkl y abcdefgh"); got != "x «secret:b» y «secret:a»" {
		t.Errorf("a value containing another must be replaced whole: %q", got)
	}
	s.SetSecrets(nil)
	if got := s.Scrub("x abcdefgh-ijkl"); got != "x abcdefgh-ijkl" {
		t.Errorf("SetSecrets(nil) must forget the values: %q", got)
	}
}

// Scrubbing runs before the value cap: a secret straddling the cut would
// otherwise leave its first part in the ring and the file.
func TestHub_ScrubsBeforeTheCap(t *testing.T) {
	known := fake("KNOWNvalue", "-0123456789xyz")
	github := fake("gh", "p_", strings.Repeat("A1", 18))
	h, root, _ := openHub(t, Options{MaxFieldBytes: 64})
	h.SetSecrets(map[string]string{"known": known})
	pad := strings.Repeat("a", 60)
	h.Log(Entry{Source: "turns", Msg: pad + known, Fields: []Field{{Key: "result", Value: pad + github}}})
	// The long-value path: scrubbed through a window past the cut.
	long := strings.Repeat("b", 60) + known + strings.Repeat("c", 3*scrubWindow)
	h.Log(Entry{Source: "turns", Msg: "long", Fields: []Field{{Key: "result", Value: long}}})
	// Replacements that shrink the window a lot fall back to a full scrub.
	dense := strings.Repeat(known, 3*scrubWindow/len(known))
	h.Log(Entry{Source: "turns", Msg: "dense", Fields: []Field{{Key: "result", Value: dense}}})

	file := readFile(t, filepath.Join(root, "turns", "2026-09-22.log"))
	all, _ := h.Snapshot()
	for _, leak := range []string{known[:4], github[:4], "ghp_"} {
		if strings.Contains(file, leak) {
			t.Errorf("file holds %q:\n%s", leak, file)
		}
		for _, e := range all {
			if strings.Contains(e.Line(), leak) {
				t.Errorf("ring holds %q: %s", leak, e.Line())
			}
		}
	}
	if v := all[1].Field("result"); !strings.HasSuffix(v, fmt.Sprintf("…(+%d bytes)", len(long)-len(known)+len("«secret:known»")-64)) {
		t.Errorf("long value cut = %q", v)
	}
}

// A field whose key names a credential loses its value, whatever it is; the
// caller's fields are left alone.
func TestHub_SecretNamedFieldsAndCallerFields(t *testing.T) {
	h := NewMemory(10)
	fields := []Field{{Key: "user", Value: "bob"}, {Key: "password", Value: "pw"}, {Key: "access_token", Value: "x"}, {Key: "tokens", Value: "12"}}
	h.Log(Entry{Source: "system", Msg: "login", Fields: fields})
	if fields[1].Value != "pw" {
		t.Error("Log must not mutate the caller's fields")
	}
	e, _ := h.Snapshot()
	if got := e[0].Line(); !strings.Contains(got, "user=bob password=«secret:password» access_token=«secret:access_token» tokens=12") {
		t.Errorf("line = %s", got)
	}
}

// Lines read back from disk (the backfill at start, another writer's folder)
// are scrubbed on their way into the ring: files written before the scrubber
// existed hold secrets.
func TestHub_ScrubsLinesReadFromDisk(t *testing.T) {
	root := t.TempDir()
	secret := strings.Repeat("dT0k-", 5)
	if err := os.MkdirAll(filepath.Join(root, "system"), 0o700); err != nil {
		t.Fatal(err)
	}
	line := "2026-09-22 13:00:00.000 INFO  [system] dashboard: serving on http://x dashboard auth token: " + secret + "\n"
	if err := os.WriteFile(filepath.Join(root, "system", "2026-09-22.log"), []byte(line+"raw "+fake("gh", "p_", strings.Repeat("Z9", 18))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := Open(root, Options{Now: (&fakeClock{t: base}).Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	h.SetSecrets(map[string]string{"dashboard-auth-token": secret})
	h.Start(t.Context())
	all, _ := h.Snapshot()
	if len(all) != 2 {
		t.Fatalf("backfill = %+v", all)
	}
	for _, e := range all {
		if strings.Contains(e.Msg, secret) || strings.Contains(e.Msg, "ghp_") {
			t.Errorf("backfilled entry not scrubbed: %q", e.Msg)
		}
	}
}

// The known set swaps while other goroutines log (run with -race).
func TestHub_SetSecretsWhileLogging(t *testing.T) {
	h := NewMemory(1000)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				h.Logf("turns", slog.LevelInfo, "tool result", "result", fmt.Sprintf("g%d-%d secret-value-%d", g, i, i%7))
			}
		}()
	}
	for i := range 50 {
		h.SetSecrets(map[string]string{"s": fmt.Sprintf("secret-value-%d", i%7)})
	}
	wg.Wait()
	h.SetSecrets(map[string]string{"s": "secret-value-3"})
	h.Logf("turns", slog.LevelInfo, "last", "result", "has secret-value-3")
	all, _ := h.Snapshot()
	if got := all[len(all)-1].Field("result"); got != "has «secret:s»" {
		t.Errorf("after the swap: %q", got)
	}
}

// Scrub and ScrubWriter use the installed hub's known values, and the shapes
// before a hub is installed.
func TestScrub_InstalledHubAndWriter(t *testing.T) {
	prev := Default()
	t.Cleanup(func() { Install(prev) })
	Install(nil)
	if got := Scrub("token=abcdef12 known-value-123"); got != "token=«secret:param» known-value-123" {
		t.Errorf("before Install: %q", got)
	}
	h := NewMemory(10)
	h.SetSecrets(map[string]string{"k": "known-value-123"})
	Install(h)
	var buf bytes.Buffer
	w := ScrubWriter(&buf)
	n, err := w.Write([]byte("level=WARN msg=x err=\"known-value-123\"\n"))
	if err != nil || n != len("level=WARN msg=x err=\"known-value-123\"\n") {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if buf.String() != "level=WARN msg=x err=\"«secret:k»\"\n" {
		t.Errorf("writer output = %q", buf.String())
	}
}

// ns/op for the common case: a turn's tool call with a handful of fields,
// nothing secret in it, twenty known values loaded.
func BenchmarkScrubEntry_TypicalLine(b *testing.B) {
	var s Scrubber
	known := map[string]string{}
	for i := range 20 {
		known[fmt.Sprintf("credentials.json:secrets.key%d", i)] = fmt.Sprintf("sk-test-%02d-AbCdEfGhIjKlMnOpQrStUv", i)
	}
	s.SetSecrets(known)
	e := Entry{Source: "turns", Level: slog.LevelInfo, Msg: "tool call web_search", Fields: []Field{
		{Key: "ev", Value: "tool_call"}, {Key: "agent", Value: "tenant"}, {Key: "iter", Value: "2"},
		{Key: "tool", Value: "web_search"}, {Key: "args", Value: `{"query":"golang slog handler examples","max_results":5}`},
	}}
	b.ReportAllocs()
	for b.Loop() {
		_ = s.Entry(e, defaultMaxField)
	}
}

// A value added while running (a secret a plugin resolved) is scrubbed, and
// it survives the credential watcher replacing the stored set.
func TestScrubber_AddSecretsSurvivesSetSecrets(t *testing.T) {
	var s Scrubber
	s.SetSecrets(map[string]string{"stored": "alpha-secret-1"})
	s.AddSecrets(map[string]string{"onepassword:op://v/i/f": "revealed-value-9", "tiny": "short"})
	if got := s.Scrub("x alpha-secret-1 y revealed-value-9 z short"); got != "x «secret:stored» y «secret:onepassword:op://v/i/f» z short" {
		t.Errorf("after AddSecrets: %q", got)
	}
	s.SetSecrets(map[string]string{"other": "bravo-secret-2"})
	if got := s.Scrub("alpha-secret-1 bravo-secret-2 revealed-value-9"); got != "alpha-secret-1 «secret:other» «secret:onepassword:op://v/i/f»" {
		t.Errorf("after SetSecrets: %q", got)
	}
	for i := 0; i < maxExtraSecrets; i++ {
		s.AddSecrets(map[string]string{fmt.Sprintf("extra-%03d", i): fmt.Sprintf("filler-value-%03d-xyz", i)})
	}
	if got := s.Scrub("revealed-value-9 filler-value-255-xyz"); got != "revealed-value-9 «secret:extra-255»" {
		t.Errorf("the oldest extra is forgotten past the cap, the newest kept: %q", got)
	}
}
